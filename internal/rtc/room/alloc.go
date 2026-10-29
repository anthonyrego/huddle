package room

import (
	"slices"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc/icept"
)

// The estimate shared among the video layers being sent. Layers get their
// bitrate lowest first (across every video published: a camera's 180p,
// then its 360p, then a screen's half size, and so on), after a reserve
// for the voice. Each may run anywhere from its floor to its full bitrate;
// a layer whose floor doesn't fit is paused, and so is every layer above
// it, since a receiver that can't have the small one can't have the big
// one either.
//
// pion's GCC raises its estimate while less is sent than it allows (it
// has no cap at the rate actually sent, as WebRTC's has), so the estimate
// layers are given is capped at 1.5 times what went out over the last
// second. That cap alone would keep a paused layer paused for good, so the
// link is probed for a paused layer's floor before it comes back. A probe
// overdrives the highest layer still running (its encoder is given more
// bits), rising from nothing to the paused layer's floor over probeFor,
// with GCC's estimate (not the cap) to say whether it fits: no new layer
// appears to the SFU, so no receiver is switched up and back down, and no
// keyframe is spent. When the link can't take it, what doesn't fit queues
// in front of the voice until GCC sees the delay grow; the slow rise keeps
// that queue short, and probes are rare: one comes only after calm
// seconds with no drop in the estimate and after the layer's hold (15 s,
// doubling with each failed try up to two minutes). The layer comes back
// at its floor once the probe has carried that much (what was sent rose
// by most of it) without a drop.
//
// A layer starting while the estimate has never dropped (a call's start
// on a good link) starts at its rate at once.
//
// Dynacast: the SFU says which layers anyone is sent (wanted; every layer
// until it says), and a layer nobody wants gets nothing. That isn't the
// estimate's doing: it isn't a failed try (no longer hold), and it doesn't
// pause the layers above it as a layer the estimate can't carry does. A
// wanted layer that isn't running has the next smaller layer stand in for
// it (needed), as the SFU stands the nearest running layer in for its
// viewers meanwhile, so a viewer is never left with nothing. A layer
// paused because nobody wanted it comes back at its floor at once when
// it's wanted again, without a probe, if it ran at its floor or more
// before, GCC's estimate (not the cap) leaves resumeMargin times its
// floor, and the estimate hasn't dropped since it paused: the link carried
// it then and nothing says it can't now. (GCC's estimate climbs while less
// is sent, so the estimate alone wouldn't say so.) A layer wanted under a
// larger one of its video that runs starts at its floor at once too, if
// the estimate leaves resumeMargin times it: the larger one shows the link
// carries more, and gives up what the smaller one takes (waiting, it would
// pause the larger one, as a layer the estimate can't carry does).
// Otherwise it takes the probe path, like a layer the estimate paused. A probe's carrier is the
// highest running layer needed, and a layer nobody needs is never probed
// for. With nothing running to carry a probe (no layer needed runs), the
// lowest needed layer starts at its floor once its hold is over and the
// estimate leaves resumeMargin times its floor, as a call's first layer
// does.

// Allocation tuning.
const (
	audioReserve = 40_000
	// floorShare is a layer's floor, as a share of its full bitrate.
	floorShare = 0.4
	// resumeMargin is how far over a paused layer's floor GCC's estimate
	// must be to probe for it (or to start it fresh).
	resumeMargin = 1.15
	// pauseShare is how far under its floor a running layer may be squeezed
	// before it pauses: GCC answers one overuse by cutting to 0.85 of what
	// arrived, so a layer at its floor would pause at the first one (a
	// keyframe's burst, say), and come back only after a probe. 0.7 rides
	// out one cut for a camera's 720p or a screen's full size.
	pauseShare = 0.7
	// probeFor is how long a probe takes to rise to the layer's floor;
	// probeFirst, while the estimate has never dropped.
	probeFor   = 8 * time.Second
	probeFirst = 4 * time.Second
	// probeCarried is how much of the floor what was sent must have risen
	// by for a probe to pass (an encoder may not use every bit given).
	probeCarried = 0.7
	// freeFor is how long a layer that started runs free of the cap, which
	// by then counts it.
	freeFor = 3 * time.Second
	// calm is how long since GCC's estimate last dropped before a probe.
	calm = 10 * time.Second
	// holdFirst and holdMost bound how long a paused layer waits.
	holdFirst = 15 * time.Second
	holdMost  = 2 * time.Minute
	// settled is how long a layer runs before a pause counts as fresh
	// news rather than a failed try.
	settled = 30 * time.Second
	// capFactor and capSlack make the cap: capFactor × sent + capSlack.
	capFactor = 1.5
	capSlack  = 100_000
	// dropBy is how far GCC's estimate falls to count as a drop (its
	// decrease on overuse is to 0.85 of what arrived; smaller dips are
	// its noise).
	dropBy = 0.85
)

// allocLayer is one layer of one published video.
type allocLayer struct {
	pub   *videoPub
	layer int
	full  int // its full bitrate

	bps       int       // what it has now (0: paused)
	since     time.Time // when it last started (zero: never)
	holdUntil time.Time
	hold      time.Duration
	probed    bool      // it came back after a probe: at its floor until the cap counts it
	added     time.Time // when its video was published

	// Dynacast: whether the SFU says anyone is sent it; whether it's
	// needed (wanted, or standing in for a larger wanted layer that isn't
	// running); whether it's paused because nobody needed it (not by the
	// estimate), and when, at what it ran before.
	wanted   bool
	needed   bool
	unwanted bool
	pausedAt time.Time
	ranAt    int
	// why it last started or stopped, for the log.
	why string
}

func (l *allocLayer) floor() int { return int(float64(l.full) * floorShare) }

// squeezed is the least a running layer is given before it pauses.
func (l *allocLayer) squeezed() int { return int(float64(l.floor()) * pauseShare) }

// free reports whether the layer started too recently for the cap.
func (l *allocLayer) free(now time.Time) bool { return !l.since.IsZero() && now.Sub(l.since) < freeFor }

// failed is a try that didn't work: the layer waits longer next time.
func (l *allocLayer) failed(now time.Time) {
	switch {
	case !l.since.IsZero() && now.Sub(l.since) >= settled:
		l.hold = holdFirst // it ran a good while: fresh news
	case !l.holdUntil.IsZero():
		l.hold = min(2*l.hold, holdMost)
	}
	l.holdUntil = now.Add(l.hold)
}

// probe is a probe running for a paused layer.
type probe struct {
	target  *allocLayer
	carrier *allocLayer
	start   time.Time
	sentAt  int           // what was sent when it began
	boost   int           // what the carrier is given over its share now
	dur     time.Duration // how long it takes to rise to the floor
}

// allocator shares the estimate among the layers.
type allocator struct {
	layers  []*allocLayer // lowest full bitrate first
	lastRaw int
	dropAt  time.Time // when GCC's estimate last dropped
	probe   *probe
}

// add puts a video's layers in, all paused and all wanted: those that fit
// start at the next allocation, the rest wait for a probe.
func (a *allocator) add(p *videoPub, full []int, now time.Time) {
	for i, b := range full {
		a.layers = append(a.layers, &allocLayer{pub: p, layer: i, full: b, hold: holdFirst, added: now, wanted: true, needed: true})
	}
	slices.SortStableFunc(a.layers, func(x, y *allocLayer) int { return x.full - y.full })
}

// want takes the SFU's word on which of a video's layers anyone is sent,
// by layer, largest first.
func (a *allocator) want(p *videoPub, wanted []bool) {
	for _, l := range a.layers {
		if l.pub == p && l.layer < len(wanted) {
			l.wanted = wanted[l.layer]
		}
	}
}

// need marks the layers each video needs: those wanted and, under a wanted
// layer that isn't running, the next smaller one, standing in for it. A
// layer paused only because nobody wanted it is taken to be coming back at
// once; if it can't, allocate makes it a paused layer like any other, and
// the next allocation has a smaller one stand in for it.
func (a *allocator) need() {
	byPub := map[*videoPub][]*allocLayer{}
	for _, l := range a.layers {
		byPub[l.pub] = append(byPub[l.pub], l)
	}
	for _, ls := range byPub {
		slices.SortFunc(ls, func(x, y *allocLayer) int { return x.layer - y.layer }) // largest first
		stand := false
		for _, l := range ls {
			l.needed = l.wanted || stand
			stand = l.needed && l.bps == 0 && !l.unwanted
		}
	}
}

// pace is the estimate video is paced from: the cap's, counting each
// layer paused because nobody wanted it at what it ran at, as though it
// still did. The cap follows what's sent: with a camera's smaller layers
// paused, a 720p frame alone, paced at 1.2 times the cap, took 18 ms of
// its 33 to leave, and capture → shown went from 20-22 ms to 27-28.
func (a *allocator) pace(raw, sent int) int {
	if sent == 0 || a.probe != nil {
		return raw
	}
	for _, l := range a.layers {
		if l.unwanted && l.bps == 0 {
			sent += l.ranAt
		}
	}
	return min(raw, int(capFactor*float64(sent))+capSlack)
}

// runsAbove reports whether a larger layer of l's video runs and is
// needed.
func (a *allocator) runsAbove(l *allocLayer) bool {
	for _, o := range a.layers {
		if o.pub == l.pub && o.layer < l.layer && o.bps > 0 && o.needed {
			return true
		}
	}
	return false
}

// resumable says whether a layer paused only because nobody wanted it may
// come back at its floor at once, with leftRaw of GCC's estimate left.
func (a *allocator) resumable(l *allocLayer, leftRaw int) bool {
	return l.unwanted && l.ranAt >= l.floor() && leftRaw >= int(resumeMargin*float64(l.floor())) && !a.dropAt.After(l.pausedAt)
}

// remove takes a video's layers out.
func (a *allocator) remove(p *videoPub) {
	a.layers = slices.DeleteFunc(a.layers, func(l *allocLayer) bool { return l.pub == p })
	if a.probe != nil && (a.probe.target.pub == p || a.probe.carrier.pub == p) {
		a.probe = nil
	}
}

// allocate shares out raw (GCC's estimate), capped by what was sent; it
// reports the estimate it used.
func (a *allocator) allocate(raw, sent int, now time.Time) int {
	if a.lastRaw > 0 && raw < int(dropBy*float64(a.lastRaw)) {
		a.dropAt = now
	}
	a.lastRaw = raw
	usable := raw
	if sent > 0 && a.probe == nil {
		usable = min(raw, int(capFactor*float64(sent))+capSlack)
	}
	quiet := a.dropAt.IsZero() || now.Sub(a.dropAt) >= calm
	left, leftRaw := usable-audioReserve, raw-audioReserve
	// What a layer may start at without a probe: the cap, or before
	// anything has been sent, the estimate GCC starts from (its own may
	// have grown for minutes while only the voice was sent).
	leftFresh := min(raw, max(usable, icept.BWEInitial)) - audioReserve
	a.need()
	running := false // something needed runs, to carry a probe
	for _, l := range a.layers {
		running = running || (l.bps > 0 && l.needed)
	}
	blocked := false
	var top *allocLayer // the highest layer running
	for _, l := range a.layers {
		was := l.bps
		if l.needed && was == 0 && l.unwanted && !blocked && !a.resumable(l, leftRaw) {
			l.unwanted = false // it waits as any paused layer does
		}
		switch {
		case !l.needed:
			l.bps = 0
		case blocked:
			l.bps = 0
		case was == 0 && l.unwanted:
			// Wanted again, and nothing says the link can't carry it: at
			// its floor at once, as after a probe.
			l.since, l.probed = now, true
			l.bps = l.floor()
			l.unwanted, l.why = false, "wanted"
		case was == 0 && a.runsAbove(l) && leftRaw >= int(resumeMargin*float64(l.floor())):
			// A larger layer of its video runs: the link carries more than
			// this one's floor, and lowest first, the larger makes room.
			// (Waiting for a probe, it would pause the larger one.)
			l.since, l.probed = now, true
			l.bps = l.floor()
			l.why = "wanted"
		case was > 0 && l.free(now):
			// Just started: GCC's estimate (not the cap) says whether it fits.
			switch {
			case leftRaw < l.squeezed():
				l.bps = 0
			case l.probed:
				l.bps = min(l.floor(), leftRaw) // what the probe showed fits
			default:
				l.bps = min(l.full, max(leftFresh, min(l.floor(), leftRaw)))
			}
		case was > 0 && left >= l.squeezed():
			l.bps = min(l.full, left)
		case was == 0 && a.probe != nil && a.probe.target == l:
			l.bps = a.probing(l, top, leftRaw, sent, now)
		case was == 0 && !now.Before(l.holdUntil) && leftRaw >= int(resumeMargin*float64(l.floor())):
			switch {
			case l.since.IsZero() && now.Sub(l.added) < time.Second && leftFresh >= l.floor():
				// Just published, and it fits what's known: at once.
				l.since, l.probed = now, false
				l.bps = min(l.full, leftFresh)
				l.why = "published"
			case quiet && top != nil && a.probe == nil:
				dur := probeFor
				if a.dropAt.IsZero() {
					dur = probeFirst // nothing has gone wrong yet: likely a good link
				}
				a.probe = &probe{target: l, carrier: top, start: now, sentAt: sent, dur: dur}
				l.bps = 0
			case !running:
				// Nothing runs to carry a probe: at its floor, as a call's
				// first layer.
				l.since, l.probed = now, true
				l.bps = l.floor()
				l.why = "started"
				running = true
			default:
				l.bps = 0
			}
		default:
			l.bps = 0
		}
		switch {
		case l.bps > 0:
			top = l
		case !l.needed:
			// Nobody needs it: Dynacast's pause, not the estimate's; the
			// layers above it carry on.
			if was > 0 {
				l.unwanted, l.pausedAt, l.ranAt, l.why = true, now, was, "unwanted"
			}
		default:
			blocked = true
			if was > 0 { // the estimate paused it
				l.failed(now)
				l.why = "bandwidth"
			}
		}
		left -= l.bps
		leftRaw -= l.bps
		leftFresh -= l.bps
	}
	if p := a.probe; p != nil {
		switch {
		case !p.target.needed || !p.carrier.needed:
			// Nobody needs one of them any more: no failure, and the
			// target, if it's still needed, tries again with another.
			a.probe = nil
		case p.carrier.bps == 0: // the carrier itself paused
			p.target.failed(now)
			a.probe = nil
		default:
			p.carrier.bps += p.boost
		}
	}
	return usable
}

// probing moves a probe for l on: it rises, fails on a drop, or passes,
// and l starts. It returns l's bitrate.
func (a *allocator) probing(l, top *allocLayer, leftRaw, sent int, now time.Time) int {
	p := a.probe
	if !p.carrier.needed {
		// Nobody needs the carrier any more: the probe ends without
		// counting against l.
		a.probe = nil
		return 0
	}
	k := min(float64(now.Sub(p.start))/float64(p.dur), 1)
	boost := int(k * float64(l.floor()))
	switch {
	case top != p.carrier || leftRaw < boost || now.Sub(p.start) > p.dur+3*time.Second:
		// The link didn't take it (or the probe couldn't finish).
		l.failed(now)
		a.probe = nil
		return 0
	case k >= 1 && sent-p.sentAt >= int(probeCarried*float64(l.floor())):
		// It carried the floor: l comes back at it.
		a.probe = nil
		l.since, l.probed = now, true
		l.why = "probed"
		return l.floor()
	}
	p.boost = boost
	return 0
}

// bitrates is what each of a video's layers has now, by layer.
func (a *allocator) bitrates(p *videoPub, n int) []int {
	out := make([]int, n)
	for _, l := range a.layers {
		if l.pub == p && l.layer < n {
			out[l.layer] = l.bps
		}
	}
	return out
}

// states are a video's layers as the log shows them, by layer: whether
// the SFU wants each, and why each that isn't running is paused
// ("unwanted": nobody needs it; "bandwidth": the estimate can't carry it,
// or it waits for a probe; "-" while it runs).
func (a *allocator) states(p *videoPub, n int) (wanted []bool, paused []string) {
	wanted, paused = make([]bool, n), make([]string, n)
	for _, l := range a.layers {
		if l.pub != p || l.layer >= n {
			continue
		}
		wanted[l.layer] = l.wanted
		switch {
		case l.bps > 0:
			paused[l.layer] = "-"
		case !l.needed || l.unwanted:
			paused[l.layer] = "unwanted"
		default:
			paused[l.layer] = "bandwidth"
		}
	}
	return wanted, paused
}

// whys are why each of a video's layers last started or stopped, by layer.
func (a *allocator) whys(p *videoPub, n int) []string {
	out := make([]string, n)
	for _, l := range a.layers {
		if l.pub == p && l.layer < n {
			out[l.layer] = l.why
		}
	}
	return out
}

// carrier is the layer a probe overdrives, if one is running.
func (a *allocator) carrier() (*videoPub, int, bool) {
	if a.probe == nil {
		return nil, 0, false
	}
	return a.probe.carrier.pub, a.probe.carrier.layer, true
}
