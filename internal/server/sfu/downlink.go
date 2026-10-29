package sfu

// The downlink: each subscriber is sent what its own connection carries.
//
// Each down connection has pion's GCC (downEstimator), fed by the viewer's
// transport-wide feedback and used only for its estimate: nothing is
// queued at the SFU (a no-op pacer, as LiveKit's default pass-through
// keeps forwarding immediate). The voice carries no transport-wide numbers,
// so the estimate is of the video. Only its delay-based part is followed:
// a narrow link queues before it drops, and loss on its own (a lossy
// network, or a simulated one, dropping without queueing) isn't a link
// too narrow for what's sent, as LiveKit's allocator doesn't take it for
// one either.
//
// A subscriber's downlink starts free: every video goes at the layer its
// viewer's settings want, as before, for as long as the link shows no sign
// of being narrow. It's limited when a queue stands in front of the link
// (the lowest one-way delay of the last moment over the lowest of the last
// ten seconds: resend.go) for overFor, with GCC's estimate under what's
// being sent. GCC alone isn't enough: an estimate that settled at the
// link's rate before more was sent doesn't drop, a queue that has filled
// and drops what overflows no longer grows, so GCC stops seeing it, and a
// lossy network's jitter reads to it as overuse. Limited (allocate), each video
// is given the largest layer, at or under what its viewer wants, that fits
// the budget, the subscriber's videos in priority order (a screen first,
// then larger tiles by the area their settings asked for, then smaller),
// the smallest layer of each before any is raised, so a lower-priority
// video goes off first (the highest-priority one is never turned off: one
// video always runs, and the estimate keeps its feedback). A layer is
// measured at the SFU (what it sent over the last rateWindow), its
// published bitrate standing in until then.
//
// The budget is GCC's estimate (or what a probe showed the link carries,
// until the next drop), less a reserve for the voices sent, and for a
// video starting (just subscribed, or turned back on) capped at capFactor
// times the video sent plus capSlack (pion's GCC climbs while less is sent
// than it allows). A layer already being sent is kept while the budget
// holds keepShare of it (GCC answers one overuse by cutting to 0.85 of
// what arrived, so a layer that fills the link would be dropped at the
// first one), all of it while the link is overused. Moving down is at
// once. Moving up is only through a probe.
//
// A probe pads one of the subscriber's videos (the one moving up, if it
// runs) with padding-only RTP packets between frames, rising to the next
// layer's rate over probeRamp. A queue standing in front of the link ends
// it at once (a keyframe's burst through a narrow link drains sooner), and
// the video waits holdFirst before its next try, doubling to holdMost.
// Probes come only after calm with no drop, and probeSettle after a video
// moved. Carried, the video moves up a layer.
//
// Once every video has had what it wants for calm, with no drop meanwhile,
// the downlink is free again: a tile made larger gets its layer at once,
// and if the link can't take it, it's limited again.
//
// Padding is the exception to the SFU sending no padding: video only, only
// while probing (and a byte of it in place of a publisher's probe filler:
// layers.go), never on a voice (padding a voice is what once pinned new
// subscribers' jitter buffers at 300 ms).

import (
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Downlink allocation tuning (the publisher's alloc.go's rules, where they
// carry over).
const (
	// allocEvery is how often each subscriber's videos are allocated again
	// (and at once when they come, go, or change settings).
	allocEvery = 500 * time.Millisecond
	// voiceReserve is kept out of the budget for each voice sent, at most
	// voicesReserved of them (people rarely talk over more).
	voiceReserve   = 40_000
	voicesReserved = 3
	// keepShare is how much of a layer being sent the budget must hold for
	// it to be kept: GCC answers one overuse by cutting to 0.85 of what
	// arrived; 0.75 rides that out.
	keepShare = 0.75
	// capFactor and capSlack make the cap on GCC's estimate: capFactor ×
	// the video sent + capSlack.
	capFactor = 1.5
	capSlack  = 100_000
	// dropBy is how far GCC's estimate falls, from its highest over the
	// last dropWithin, to count as a drop: while it sees the link overused
	// it's cut to 0.85 of what arrived at each feedback, and what arrived
	// may come down a step at a time.
	dropBy     = 0.85
	dropWithin = time.Second
	// queueMost is the queue that, standing in front of the link for
	// overFor with GCC's estimate under queueShare of what's sent, limits a
	// free downlink (a queue is the link full, and GCC's estimate 0.85 of
	// what gets through).
	queueMost  = 30 * time.Millisecond
	overFor    = 100 * time.Millisecond
	queueShare = 0.9
	// calm is how long since GCC's estimate last dropped before a probe,
	// and how long everything must have had what it wants to be free.
	calm = 10 * time.Second
	// probeRamp is how long a probe's padding takes to rise to what moving
	// up adds; probeLate is how much longer it may take to carry it.
	probeRamp = 5 * time.Second
	probeLate = 3 * time.Second
	// probeQueue ends a probe: a queue that long standing in front of the
	// link for probeQueueFor (the voice waits in it too), or at all while
	// GCC's estimate falls.
	probeQueue    = 10 * time.Millisecond
	probeQueueFor = 50 * time.Millisecond
	// probeSettle is how long after a video moves before a probe: its new
	// layer's keyframe has gone through.
	probeSettle = 2 * time.Second
	// probeSlip ends a probe: GCC's estimate falling under this share of its
	// highest since the probe began.
	probeSlip = 0.95
	// probeCarried is how much of what moving up adds the padding must have
	// carried over the last second for a probe to pass.
	probeCarried = 0.7
	// holdFirst and holdMost bound how long a video waits after a failed
	// probe; settled is how long a layer must have run for a failure to
	// count as fresh news rather than another failed try.
	holdFirst = 15 * time.Second
	holdMost  = 2 * time.Minute
	settled   = 30 * time.Second
	// The down estimators' range and where they start: high, since the
	// estimate means nothing until the link has pushed back (and a free
	// downlink is limited only when it's under what's sent).
	downInitial = 20_000_000
	downMin     = 100_000
	downMax     = 50_000_000
)

// Padding and measuring.
const (
	// rtpOverhead is what each packet adds to its payload as the
	// estimate counts it: the header and the transport-wide number.
	rtpOverhead = 20
	// padSize is the padding a packet carries: RTP's most.
	padSize   = 255
	padPacket = padSize + rtpOverhead
	// padBurst caps the padding packets after one frame, and padSaved how
	// much padding time a video may save up between frames.
	padBurst = 40
	padSaved = 100 * time.Millisecond
	// rateWindow is how far back a layer's rate is measured.
	rateWindow = 2 * time.Second
)

// limOff and limNew are a down track's lim when its downlink says none of
// its layers: turned off, or not allocated yet (just subscribed, or turned
// back on, while limited).
const (
	limOff = -1
	limNew = -2
)

// rateMeter counts bytes over a window, in 100 ms bins.
type rateMeter struct {
	mu     sync.Mutex
	bins   []int
	newest time.Time // the start of the newest bin
	i      int
	since  time.Time // when it began counting (again, after a window of nothing)
}

func newRateMeter(window time.Duration) *rateMeter {
	return &rateMeter{bins: make([]int, window/(100*time.Millisecond))}
}

func (m *rateMeter) roll(now time.Time) {
	if m.newest.IsZero() {
		m.newest = now
		return
	}
	if now.Sub(m.newest) >= time.Duration(len(m.bins))*100*time.Millisecond {
		clear(m.bins)
		m.newest, m.since = now, time.Time{}
		return
	}
	for now.Sub(m.newest) >= 100*time.Millisecond {
		m.newest = m.newest.Add(100 * time.Millisecond)
		m.i = (m.i + 1) % len(m.bins)
		m.bins[m.i] = 0
	}
}

func (m *rateMeter) add(now time.Time, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll(now)
	if m.since.IsZero() {
		m.since = now
	}
	m.bins[m.i] += n
}

// rate is the bits a second over the window (or since counting began, if
// that's shorter); ok once it has counted for a second.
func (m *rateMeter) rate(now time.Time) (bps int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll(now)
	if m.since.IsZero() || now.Sub(m.since) < time.Second {
		return 0, false
	}
	sum := 0
	for _, b := range m.bins {
		sum += b
	}
	span := min(now.Sub(m.since), time.Duration(len(m.bins))*100*time.Millisecond)
	return int(float64(sum*8) / span.Seconds()), true
}

// layerRate is layer i's bitrate as the SFU receives it, or its published
// bitrate before it has been measured.
func (pub *publication) layerRate(i int, now time.Time) int {
	l := pub.layers[i]
	if bps, ok := l.meter.rate(now); ok {
		return bps
	}
	return l.info.Bitrate
}

// downEstimator is a down connection's bandwidth estimate: pion's GCC with
// nothing paced, counting the video the connection sends (padding and
// resends too).
type downEstimator struct {
	*gcc.SendSideBWE
	video    *rateMeter
	gate     *gate                  // resend.go
	onChange atomic.Pointer[func()] // told when GCC's estimate changes
}

// estimate is GCC's delay-based estimate (its own is the lower of that and
// its loss-based one).
func (e *downEstimator) estimate() int {
	if v, ok := e.GetStats()["delayTargetBitrate"].(int); ok && v > 0 {
		return v
	}
	return e.GetTargetBitrate()
}

func (e *downEstimator) AddStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	next := e.SendSideBWE.AddStream(info, w)
	if !strings.HasPrefix(strings.ToLower(info.MimeType), "video/") {
		return next
	}
	var twcc uint8
	for _, x := range info.RTPHeaderExtensions {
		if x.URI == sdp.TransportCCURI {
			twcc = uint8(x.ID)
		}
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		now := time.Now()
		e.video.add(now, len(payload)+int(h.PaddingSize)+rtpOverhead)
		e.gate.wrote(h, twcc, now)
		return next.Write(h, payload, a)
	})
}

// newDownCC is the congestion control on every down connection; s keeps
// each connection's estimator, by the connection's id, until the
// connection takes it (pion makes the estimator inside NewPeerConnection,
// under the id the connection then reports).
func newDownCC(s *Server) (*cc.InterceptorFactory, error) {
	f, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		bwe, err := gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(downInitial), gcc.SendSideBWEMinBitrate(downMin),
			gcc.SendSideBWEMaxBitrate(downMax), gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
		if err != nil {
			return nil, err
		}
		e := &downEstimator{SendSideBWE: bwe, video: newRateMeter(time.Second), gate: newGate()}
		bwe.OnTargetBitrateChange(func(int) {
			if f := e.onChange.Load(); f != nil {
				(*f)()
			}
		})
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	f.OnNewPeerConnection(func(id string, e cc.BandwidthEstimator) {
		if de, ok := e.(*downEstimator); ok {
			s.estMu.Lock()
			s.ests[id] = de
			s.estMu.Unlock()
		}
	})
	return f, nil
}

// takeEstimator is the down connection id's estimator (nil if its API has
// none: a test's).
func (s *Server) takeEstimator(id string) *downEstimator {
	s.estMu.Lock()
	defer s.estMu.Unlock()
	e := s.ests[id]
	delete(s.ests, id)
	return e
}

// downlink is one subscriber's allocation.
type downlink struct {
	p   *participant
	est *downEstimator // nil: no estimate, and everything asked for is sent

	limited atomic.Bool // GCC's estimate dropped below what's sent: videos are fitted to it
	dirty   atomic.Bool // videos came, went or changed: allocate at the next tick

	mu        sync.Mutex
	raw       int       // GCC's estimate, as last seen
	recent    []rawAt   // its values over the last dropWithin
	proven    int       // the video a probe carried, until the link pushes back
	queuedAt  time.Time // since when a queue has stood in front of the link
	movedAt   time.Time // when a video last moved
	dropAt    time.Time // when it last dropped
	wantedAt  time.Time // when a video last had less than it wants
	allocAt   time.Time
	logAt     time.Time
	probe     *downProbe
	padded    *rateMeter // the probe's padding, as sent
	allocated int        // what the last allocation gave the videos, bits a second
}

// rawAt is GCC's estimate at a moment.
type rawAt struct {
	raw int
	at  time.Time
}

// downProbe is a probe running for one video's next layer up.
type downProbe struct {
	track    *downTrack // moving up
	carrier  *downTrack // padded
	from     int        // its lim when the probe began
	to       int        // the layer it moves up to
	extra    int        // what that adds, bits a second
	start    time.Time
	peak     int       // GCC's estimate's highest since it began
	queuedAt time.Time // since when a queue has stood in front of the link
}

func newDownlink(p *participant, est *downEstimator) *downlink {
	dl := &downlink{p: p, est: est, padded: newRateMeter(time.Second)}
	if est != nil {
		f := dl.onEstimate
		est.onChange.Store(&f)
	}
	return dl
}

// dlState is a down track's place in its downlink's allocation (guarded by
// downlink.mu).
type dlState struct {
	holdUntil time.Time
	hold      time.Duration
	since     time.Time // when it last moved up through a probe
}

// limited is the layer a viewer wanting want may be sent, given what its
// downlink carries: want, or the smaller layer the allocation allows
// (layers run largest first, so the larger index); -1 for none.
func (d *downTrack) limited(want int) int {
	lim := int(d.lim.Load())
	if want < 0 || lim < 0 {
		return -1
	}
	return max(want, lim)
}

// padding is a padding-only packet's payload: RTP padding written out, its
// last byte its length. Written as the header's PaddingSize instead, the
// padding is what's left in SRTP's pooled buffer (another packet's
// ciphertext), and pion's GCC counts the packet at its header's size, so
// what a probe adds never shows in the rate GCC measures (and cuts to 0.85
// of on an overuse). In the payload, it's zeros and counted; the far end
// reads it the same (the padding bit, the length in the last byte), and
// pion's NACK responder keeps it as it keeps any packet (its packet store
// handles padding written this way, which older pion/rtp required).
var padding = func() []byte {
	b := make([]byte, padSize)
	b[padSize-1] = padSize
	return b
}()

// padLocked pads after the frame just forwarded, while the track carries a
// probe: as many padding-only packets as the probe's rate has owed since
// the last frame (at most padBurst), each numbered in the forwarder's run
// with that frame's timestamp. d.mu is held.
func (d *downTrack) padLocked(now time.Time) {
	rate := d.padRate.Load()
	if rate <= 0 {
		d.padAt, d.padOwed = time.Time{}, 0
		return
	}
	if !d.padAt.IsZero() {
		d.padOwed += now.Sub(d.padAt).Seconds() * float64(rate) / 8
	}
	d.padAt = now
	d.padOwed = min(d.padOwed, padSaved.Seconds()*float64(rate)/8)
	n := min(int(d.padOwed/padPacket), padBurst)
	first, ts, ok := d.fwd.pad(n)
	if !ok {
		return
	}
	sent := 0
	for i := range n {
		h := rtp.Header{Version: 2, Padding: true, PayloadType: d.pt, SequenceNumber: first + uint16(i), Timestamp: ts, SSRC: d.ssrc}
		if _, err := d.w.WriteRTP(&h, padding); err != nil {
			break
		}
		d.packets++
		sent++
	}
	d.padOwed -= float64(n * padPacket)
	d.sub.dl.padded.add(now, sent*padPacket)
}

// reserve is what the voices the subscriber is sent are kept.
func reserveFor(voices int) int { return voiceReserve * min(voices, voicesReserved) }

// dlTrack is one video in an allocation.
type dlTrack struct {
	dt      *downTrack
	want    int // the layer its viewer wants
	aimed   int // the layer it's being sent (-1: none)
	lim     int
	running []int // the layers that run, largest first
	screen  bool
	area    int64
	sending bool // its forwarder sends a layer
}

// videos are the subscriber's video down tracks on its connection that
// its viewer wants, in priority order, and how many voices it's sent.
func (dl *downlink) videos() ([]dlTrack, int) {
	r := dl.p.rm.Load()
	if r == nil {
		return nil, 0
	}
	r.mu.Lock()
	dts := make([]*downTrack, 0, len(dl.p.downs))
	for _, dt := range dl.p.downs {
		dts = append(dts, dt)
	}
	r.mu.Unlock()
	var out []dlTrack
	voices := 0
	for _, dt := range dts {
		if dt.fwd == nil {
			if !dt.pub.muted.Load() {
				voices++
			}
			continue
		}
		dt.mu.Lock()
		t := dlTrack{dt: dt, want: dt.want, aimed: dt.aimed, lim: int(dt.lim.Load()), sending: dt.fwd.current >= 0 && dt.packets > 0}
		bound := dt.bound
		dt.mu.Unlock()
		if !bound {
			continue
		}
		if t.want < 0 {
			if t.lim != limNew && dl.limited.Load() {
				dt.lim.Store(limNew) // turned back on, it starts at what fits
			}
			continue
		}
		for i, l := range dt.pub.layers {
			if l.alive.Load() {
				t.running = append(t.running, i)
			}
		}
		if len(t.running) == 0 {
			continue
		}
		t.screen = dt.pub.req.Source == signal.Screen
		t.area = dt.area.Load()
		if t.area == 0 {
			li := dt.pub.layers[min(t.want, len(dt.pub.layers)-1)].info
			t.area = int64(li.W * li.H)
		}
		out = append(out, t)
	}
	sortVideos(out)
	return out, voices
}

// sortVideos puts videos in priority order: a screen first, then larger
// tiles, then smaller; among equals, the one published first.
func sortVideos(vs []dlTrack) {
	slices.SortStableFunc(vs, func(a, b dlTrack) int {
		switch {
		case a.screen != b.screen:
			if a.screen {
				return -1
			}
			return 1
		case a.area != b.area:
			if a.area > b.area {
				return -1
			}
			return 1
		}
		return int(a.dt.pub.n - b.dt.pub.n)
	})
}

// onEstimate is GCC's estimate changing: a drop is acted on at once.
func (dl *downlink) onEstimate() {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	now := time.Now()
	if dl.estimate(now) {
		dl.allocate(now)
	}
}

// tick is the room's clock: the estimate looked at, the videos allocated
// every allocEvery (or when they've changed), and the probe moved on.
func (dl *downlink) tick(now time.Time) {
	if dl.est == nil {
		return
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	again := dl.estimate(now)
	if again || dl.dirty.Load() || now.Sub(dl.allocAt) >= allocEvery {
		dl.allocate(now)
	}
	dl.probing(now)
	if now.Sub(dl.logAt) >= 5*time.Second {
		dl.logAt = now
		sent, _ := dl.est.video.rate(now)
		dl.p.log.Debug("sfu: downlink", "limited", dl.limited.Load(), "estimate", dl.raw, "sent", sent, "allocated", dl.allocated)
	}
}

// changed is the subscriber's videos changing (a settings): allocated at
// once.
func (dl *downlink) changed(now time.Time) {
	if dl == nil || dl.est == nil {
		return
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	dl.allocate(now)
}

// estimate reads GCC's estimate; it reports whether the videos need
// allocating again (it dropped). dl.mu is held.
func (dl *downlink) estimate(now time.Time) bool {
	raw := dl.est.estimate()
	queue := dl.est.gate.queue()
	high := 0
	dl.recent = slices.DeleteFunc(dl.recent, func(r rawAt) bool { return now.Sub(r.at) > dropWithin })
	for _, r := range dl.recent {
		high = max(high, r.raw)
	}
	drop := raw < int(dropBy*float64(high))
	if drop {
		dl.recent = dl.recent[:0] // a drop counts once
	}
	dl.recent = append(dl.recent, rawAt{raw, now})
	dl.raw = raw
	if p := dl.probe; p != nil {
		// A probe the link can't carry leaves a queue standing in front of
		// it; GCC's estimate may fall for a keyframe's burst through a
		// narrow link, or for jitter (inflated while less was sent, one
		// overuse takes it to 0.85 of what arrived).
		p.peak = max(p.peak, raw)
		switch {
		case queue < probeQueue:
			p.queuedAt = time.Time{}
		case p.queuedAt.IsZero():
			p.queuedAt = now
		}
		switch {
		case !p.queuedAt.IsZero() && now.Sub(p.queuedAt) >= probeQueueFor:
			dl.endProbe(now, false, "a queue stood")
		case !p.queuedAt.IsZero() && raw < int(probeSlip*float64(p.peak)):
			dl.endProbe(now, false, "the estimate fell")
		}
	}
	if drop {
		dl.dropAt, dl.proven = now, 0
	}
	// Free, the downlink is limited by a queue standing in front of the
	// link for overFor, the estimate well under what's being sent. Not by
	// GCC alone: its delay-based part sees a queue only while it grows (an
	// estimate settled at the link's rate before more was sent, a larger
	// layer or a publisher's probe raising one, doesn't drop, and a queue
	// that has filled holds still), and takes a lossy network's jitter for
	// overuse, cutting to 0.85 of what arrived, which loss makes less than
	// what was sent.
	switch {
	case queue < queueMost:
		dl.queuedAt = time.Time{}
	case dl.queuedAt.IsZero():
		dl.queuedAt = now
	}
	queued := !dl.queuedAt.IsZero() && now.Sub(dl.queuedAt) >= overFor
	sent, _ := dl.est.video.rate(now)
	if queued {
		dl.proven = 0 // the link pushes back: what a probe showed is past
	}
	if !dl.limited.Load() && queued {
		// What's being sent: measured, or (a layer just switched to) the
		// layers' measured rates. (A layer its publisher just started runs
		// at its floor, far under its published bitrate.)
		vs, _ := dl.videos()
		layers := 0
		for _, v := range vs {
			if v.aimed < 0 {
				continue
			}
			if bps, ok := v.dt.pub.layers[v.aimed].meter.rate(now); ok {
				layers += bps
			}
		}
		if float64(raw) >= queueShare*float64(max(sent, layers)) {
			return drop // GCC hasn't seen it yet
		}
		dl.limited.Store(true)
		dl.dropAt = now
		dl.p.log.Info("sfu: downlink limited", "estimate", raw, "sent", sent, "layers", layers, "queue", queue.Round(time.Millisecond))
		return true
	}
	return drop
}

// allocate gives each video its layer. dl.mu is held.
func (dl *downlink) allocate(now time.Time) {
	dl.dirty.Store(false)
	dl.allocAt = now
	vs, voices := dl.videos()
	if !dl.limited.Load() {
		dl.allocated = 0
		for _, v := range vs {
			v.dt.setLimit(0, now)
		}
		return
	}
	reserve := reserveFor(voices)
	sent, _ := dl.est.video.rate(now)
	// What's being sent may keep what GCC's estimate, or a probe passed
	// since its last drop, says the link carries (GCC's estimate climbs
	// slowly after a drop: near what arrived when it cut, a packet's worth
	// at a time).
	keepLeft := max(dl.raw, dl.proven) - reserve
	growLeft := min(dl.raw, int(capFactor*float64(sent))+capSlack) - reserve // for a video starting
	// While a queue stands in front of the link, what's sent doesn't fit,
	// whatever share of it the budget holds: a
	// layer whose encoder is still rising (measured over the last
	// rateWindow) is larger than it's measured.
	keep := keepShare
	if !dl.queuedAt.IsZero() {
		keep = 1
	}
	got := fitVideos(vs, keepLeft, growLeft, keep, func(dt *downTrack, i int) int { return dt.pub.layerRate(i, now) })
	capped := false
	dl.allocated = 0
	for i, v := range vs {
		lim := got[i]
		if lim < 0 && i == 0 {
			lim = v.running[len(v.running)-1] // the first is never turned off
		}
		if lim < 0 {
			lim = limOff
		} else {
			dl.allocated += v.dt.pub.layerRate(lim, now)
		}
		if lim == limOff || lim > v.want {
			capped = true
		}
		if lim != v.lim {
			dl.movedAt = now
			dl.p.log.Info("sfu: downlink moves a video", "track", v.dt.pub.sid, "layer", v.dt.ridOf(lim), "was", v.dt.ridOf(v.lim),
				"wants", v.dt.ridOf(v.want), "estimate", dl.raw, "sent", sent)
		}
		v.dt.setLimit(lim, now)
	}
	if capped || dl.wantedAt.IsZero() {
		dl.wantedAt = now
	}
	if dl.probe == nil && now.Sub(dl.wantedAt) >= calm && now.Sub(dl.dropAt) >= calm {
		dl.limited.Store(false)
		dl.p.log.Info("sfu: downlink free", "estimate", dl.raw, "sent", sent)
		for _, v := range vs {
			v.dt.setLimit(0, now)
		}
	}
}

// fitVideos is each video's layer (-1: off), in vs's order: the smallest
// layer of each, in order, while it fits, then each raised, in order, as
// far as it can go and fits. A video may go no higher than its viewer
// wants nor than it's allowed now (moving up is a probe's); one starting
// (limNew) may go as high as its viewer wants. Keeping the layer being
// sent costs keep of it (keepShare, or all of it while the link is
// overused); anything else, all of it. keepLeft is the budget for what's
// being sent, growLeft for a video starting.
func fitVideos(vs []dlTrack, keepLeft, growLeft int, keep float64, rate func(*downTrack, int) int) []int {
	got := make([]int, len(vs))
	cost := func(v dlTrack, i int) int {
		r := rate(v.dt, i)
		if i == v.aimed && v.lim != limNew {
			return int(keep * float64(r))
		}
		return r
	}
	ceiling := func(v dlTrack) int {
		switch v.lim {
		case limNew:
			return v.want
		case limOff:
			return math.MaxInt // nothing: it comes back through a probe
		}
		return max(v.want, v.lim)
	}
	fits := func(v dlTrack, c int) bool { return c <= keepLeft && (v.lim != limNew || c <= growLeft) }
	charge := func(c int) { keepLeft, growLeft = keepLeft-c, growLeft-c }
	for k, v := range vs {
		got[k] = -1
		smallest := v.running[len(v.running)-1]
		if smallest < ceiling(v) {
			continue
		}
		if c := cost(v, smallest); fits(v, c) {
			got[k] = smallest
			charge(c)
		}
	}
	for k, v := range vs {
		if got[k] < 0 {
			continue
		}
		have := cost(v, got[k])
		for _, i := range v.running { // largest first
			if i >= got[k] {
				break
			}
			if i < ceiling(v) {
				continue
			}
			if c := cost(v, i) - have; fits(v, c) {
				got[k] = i
				charge(c)
				break
			}
		}
	}
	return got
}

// setLimit sets the largest layer the downlink allows the track (lim, or
// limOff), and chooses its layer again if that changed.
func (d *downTrack) setLimit(lim int, now time.Time) {
	if int(d.lim.Swap(int32(lim))) != lim {
		d.retarget(now, true)
	}
}

// probing moves a running probe on, or starts one. dl.mu is held.
func (dl *downlink) probing(now time.Time) {
	if p := dl.probe; p != nil {
		k := min(float64(now.Sub(p.start))/float64(probeRamp), 1)
		on := false
		if r := dl.p.rm.Load(); r != nil {
			r.mu.Lock()
			on = dl.p.downs[p.track.pub.sid] == p.track && dl.p.downs[p.carrier.pub.sid] == p.carrier
			r.mu.Unlock()
		}
		padded, _ := dl.padded.rate(now)
		switch {
		case !on || !dl.limited.Load():
			dl.endProbe(now, false, "its video went")
		case int(p.track.lim.Load()) != p.from:
			dl.endProbe(now, false, "its video moved")
		case k >= 1 && float64(padded) >= probeCarried*float64(p.extra):
			dl.endProbe(now, true, "carried")
		case now.Sub(p.start) > probeRamp+probeLate:
			dl.endProbe(now, false, "not carried")
		default:
			p.carrier.padRate.Store(int64(k * float64(p.extra)))
		}
		return
	}
	if !dl.limited.Load() || now.Sub(dl.dropAt) < calm || now.Sub(dl.movedAt) < probeSettle {
		return
	}
	vs, _ := dl.videos()
	var carrier *downTrack
	for _, v := range vs {
		if v.sending && (carrier == nil || !v.screen) {
			carrier = v.dt // a camera's frames come more often than a screen's
			if !v.screen {
				break
			}
		}
	}
	for _, v := range vs {
		// The next layer up from what it has, no larger than it wants: the
		// smallest larger running layer (its smallest, if it's off).
		have, next := v.lim, -1
		switch {
		case have == limNew:
			continue
		case have == limOff:
			next = v.running[len(v.running)-1]
		default:
			for _, i := range v.running { // largest first
				if i < have && i >= v.want {
					next = i
				}
			}
		}
		if next < 0 || now.Before(v.dt.dl.holdUntil) {
			continue
		}
		extra := v.dt.pub.layerRate(next, now)
		if have >= 0 {
			extra -= v.dt.pub.layerRate(have, now)
		}
		if extra <= 0 {
			continue
		}
		c := carrier
		if v.sending {
			c = v.dt
		}
		if c == nil {
			return
		}
		dl.probe = &downProbe{track: v.dt, carrier: c, from: have, to: next, extra: extra, start: now, peak: dl.raw}
		dl.p.log.Info("sfu: downlink probe", "track", v.dt.pub.sid, "to", v.dt.ridOf(next), "from", v.dt.ridOf(have),
			"adds", extra, "carrier", c.pub.sid, "estimate", dl.raw, "allocated", dl.allocated)
		return
	}
}

// endProbe ends the probe running: carried, its video moves up a layer;
// not, it waits. dl.mu is held.
func (dl *downlink) endProbe(now time.Time, carried bool, why string) {
	p := dl.probe
	dl.probe = nil
	p.carrier.padRate.Store(0)
	padded, _ := dl.padded.rate(now)
	st := &p.track.dl
	if carried {
		st.since = now
	} else {
		switch {
		case st.hold == 0 || (!st.since.IsZero() && now.Sub(st.since) >= settled):
			st.hold = holdFirst
		default:
			st.hold = min(2*st.hold, holdMost)
		}
		st.holdUntil = now.Add(st.hold)
	}
	hold := st.hold
	took := now.Sub(p.start).Round(time.Millisecond)
	if !carried {
		dl.p.log.Info("sfu: downlink probe failed", "track", p.track.pub.sid, "to", p.track.ridOf(p.to), "why", why,
			"after", took, "padded", padded, "adds", p.extra, "estimate", dl.raw, "peak", p.peak, "hold", hold, "usage", dl.est.GetStats()["usage"])
		return
	}
	dl.proven, _ = dl.est.video.rate(now)
	dl.p.log.Info("sfu: downlink probe passed", "track", p.track.pub.sid, "to", p.track.ridOf(p.to), "after", took,
		"padded", padded, "adds", p.extra, "estimate", dl.raw, "carried", dl.proven)
	p.track.setLimit(p.to, now)
	dl.movedAt = now
	dl.dirty.Store(true)
}
