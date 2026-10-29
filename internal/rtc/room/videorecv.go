package room

// Receiving video: each remote camera or screen is read as raw RTP, put
// back together into access units, held for lip sync, decoded with the
// app's decoder and kept in a one-frame slot the window takes from.

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/video"
)

// pliEvery is the least time between two keyframe requests for one track.
const pliEvery = 500 * time.Millisecond

// Lip sync bounds.
const (
	// maxHold is the longest a frame waits for its audio; a frame further
	// ahead than that is shown at once (the clocks disagree).
	maxHold = 500 * time.Millisecond
	// audioStale is how long since the audio last played (not counting
	// silences, which it goes on through on the wall clock) that it no
	// longer counts as playing.
	audioStale = 10 * time.Second
)

// videoTrack is one remote camera or screen (rtc.VideoTrack).
type videoTrack struct {
	r     *room
	who   string
	kind  rtc.VideoKind
	ctl   remoteVideo
	track *webrtc.TrackRemote
	dec   rtc.VideoDecoder
	sr    srClock

	latest video.Latest
	aus    chan au
	done   chan struct{}
	// wantKey tells the reading goroutine's assembler that the decoder
	// needs a keyframe (after a decode error, or shown again).
	wantKey atomic.Bool

	// The size asked for and the one sent to the SFU: -1 before any, so a
	// track never shown is turned off by its first SetSize(0, 0).
	sizeMu   sync.Mutex
	wantW    int
	wantH    int
	sentW    int
	sentH    int
	disabled bool
	sizeAt   time.Time
	sizeT    *time.Timer

	mu       sync.Mutex
	st       VideoTrackStats
	lastPLI  time.Time
	waitKey  bool
	lostAt   time.Time // when the loss being mended was found
	subAt    time.Time
	holdSum  time.Duration
	holdN    int
	lastSize [2]int
	shown    bool   // a frame has been shown
	shownTS  uint32 // the RTP timestamp of the last one
}

// VideoTrackStats are a remote video's numbers.
type VideoTrackStats struct {
	Identity   string
	Kind       rtc.VideoKind
	W, H       int // the last frame's size
	Frames     int // decoded and shown
	Keyframes  int
	Units      int // access units put together
	Skipped    int // units dropped while waiting for a keyframe
	Gaps       int // losses that made it wait for one
	PLIs       int // keyframes asked for
	DecodeErrs int
	LostPkts   int // packets given up for good
	Cut        int // units cut off by the next (a layer switch), dropped
	Stale      int // frames decoded but not shown: no newer than the last shown
	// Recoveries are how long each loss took to mend: from the gap found
	// to the next keyframe shown.
	Recoveries []time.Duration
	Held       time.Duration // the average lip-sync hold
	FirstFrame time.Duration // from subscribing to the first frame shown
}

func (t *videoTrack) Identity() string    { return t.who }
func (t *videoTrack) Kind() rtc.VideoKind { return t.kind }
func (t *videoTrack) Take(after uint64, fn func(video.Frame)) (uint64, bool) {
	return t.latest.Take(after, fn)
}

// sizeEvery is the least time between two size requests sent to the SFU;
// a resize in between sends the last one when it's due.
const sizeEvery = 250 * time.Millisecond

// SetSize asks the SFU for the layer that fits w×h (0, 0: none; the
// track is turned off until shown again).
func (t *videoTrack) SetSize(w, h int) {
	t.sizeMu.Lock()
	defer t.sizeMu.Unlock()
	if w == t.wantW && h == t.wantH {
		return
	}
	t.wantW, t.wantH = w, h
	if wait := sizeEvery - time.Since(t.sizeAt); wait > 0 {
		if t.sizeT == nil {
			t.sizeT = time.AfterFunc(wait, t.sendSize)
		}
		return
	}
	go t.sendSize()
}

// sendSize sends the size wanted now.
func (t *videoTrack) sendSize() {
	t.sizeMu.Lock()
	t.sizeT = nil
	w, h := t.wantW, t.wantH
	if w == t.sentW && h == t.sentH {
		t.sizeMu.Unlock()
		return
	}
	t.sentW, t.sentH, t.sizeAt = w, h, time.Now()
	off := w <= 0 || h <= 0
	was := t.disabled
	t.disabled = off
	t.sizeMu.Unlock()
	switch {
	case off:
		t.ctl.setEnabled(false)
	default:
		t.ctl.setSize(w, h)
		if was {
			// Shown again: the SFU forwards from the next keyframe, so ask
			// for one rather than wait for it (once the SFU has the
			// setting), and decode nothing before it.
			t.ctl.setEnabled(true)
			t.mu.Lock()
			t.waitKey = true
			t.mu.Unlock()
			t.wantKey.Store(true)
			time.AfterFunc(100*time.Millisecond, t.askKey)
		}
	}
	slog.Info("video: size asked", "identity", t.who, "kind", t.kind, "w", w, "h", h)
}

// subscribedVideo starts showing a video track we're sent, and returns
// what reads its RTCP.
func (r *room) subscribedVideo(rt remoteTrack) func(rtcp.Packet) {
	if !strings.EqualFold(rt.mime, webrtc.MimeTypeH264) {
		slog.Warn("video: not H.264, not shown", "identity", rt.who, "codec", rt.mime)
		return nil
	}
	if r.cfg.NewDecoder == nil {
		slog.Info("video: no decoder; not shown", "identity", rt.who)
		return nil
	}
	dec, err := r.cfg.NewDecoder()
	if err != nil {
		slog.Error("video: decoder", "err", err)
		return nil
	}
	kind := rtc.Camera
	if rt.source == sourceScreen {
		kind = rtc.Screen
	}
	t := &videoTrack{r: r, who: rt.who, kind: kind, ctl: rt.video, track: rt.track, dec: dec,
		aus: make(chan au, 64), done: make(chan struct{}), waitKey: true, subAt: time.Now(),
		wantW: -1, wantH: -1, sentW: -1, sentH: -1}
	t.st.Identity, t.st.Kind = t.who, kind
	r.mu.Lock()
	r.videos[rt.sid] = t
	r.mu.Unlock()
	slog.Info("video: receiving", "identity", t.who, "kind", kind, "track", rt.sid)
	go t.read()
	go t.play()
	if r.ev.VideoAdded != nil {
		r.ev.VideoAdded(t)
	}
	return t.sr.read
}

func (r *room) unsubscribedVideo(sid string) {
	r.mu.Lock()
	t := r.videos[sid]
	delete(r.videos, sid)
	r.mu.Unlock()
	if t == nil {
		return
	}
	t.logStats()
	if r.ev.VideoRemoved != nil {
		r.ev.VideoRemoved(t)
	}
}

// read takes the track's packets, puts units together and passes them on,
// dropping what can't be decoded until a keyframe comes.
func (t *videoTrack) read() {
	defer close(t.aus)
	asm := newAssembler()
	pktHook, unitHook := t.r.cfg.VideoPacket, t.r.cfg.VideoUnit
	skipped := 0 // units the decoder couldn't take, besides what the assembler skipped
	for {
		p, attr, err := t.track.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		if pktHook != nil {
			wire, _ := icept.Arrived(attr)
			pktHook(t, p, wire, now)
		}
		if len(p.Payload) > 0 { // not the SFU's padding
			t.sr.arrived(p.Timestamp, now, 90000)
		}
		if t.wantKey.Swap(false) {
			asm.awaitKey(now)
		}
		for _, u := range asm.push(p, now) {
			t.mu.Lock()
			t.st.Units++
			if u.gap && !t.waitKey {
				t.st.Gaps++
				t.waitKey, t.lostAt = true, u.lost
				if t.lostAt.IsZero() {
					t.lostAt = now
				}
			}
			skip := t.waitKey && !u.key
			if skip {
				skipped++
			} else {
				t.waitKey = false
			}
			t.mu.Unlock()
			if unitHook != nil {
				unitHook(t, VideoUnitInfo{RTP: u.ts, Key: u.key, Gap: u.gap, Skipped: skip, Lost: u.lost, Last: u.last, At: u.at})
			}
			if skip {
				t.askKey()
				continue
			}
			select {
			case t.aus <- u:
			default: // the decoder is far behind: start again at a keyframe
				t.mu.Lock()
				t.waitKey, t.lostAt = true, now
				t.st.Gaps++
				t.mu.Unlock()
				asm.awaitKey(now)
				t.askKey()
			}
		}
		// The assembler gave a hole up: ask for a keyframe now, not when
		// the next unit comes (it drops what isn't one meanwhile).
		waiting := asm.awaitingKey()
		t.mu.Lock()
		t.st.LostPkts, t.st.Cut, t.st.Skipped = asm.Dropped, asm.Cut, skipped+asm.Skipped
		if waiting && !t.waitKey {
			t.st.Gaps++
			t.waitKey, t.lostAt = true, asm.lostAt
		}
		t.mu.Unlock()
		if waiting {
			t.askKey()
		}
	}
}

// askKey asks the sender for a keyframe, once per pliEvery.
func (t *videoTrack) askKey() {
	t.mu.Lock()
	if time.Since(t.lastPLI) < pliEvery {
		t.mu.Unlock()
		return
	}
	t.lastPLI = time.Now()
	t.st.PLIs++
	t.mu.Unlock()
	t.ctl.pli()
}

// staleMost is how far behind the last frame shown (in 90 kHz ticks) a
// frame can be and still be old news; further back, the sender's clock
// started over, and the frame is shown.
const staleMost = 5 * 90000

// play holds each unit until the same person's audio reaches it, decodes
// it and shows it if it's newer than the last frame shown. An older one
// (the SFU switched to a layer behind the last, and sent a few frames
// again from its keyframe) is decoded, for the frames after it, and not
// shown.
func (t *videoTrack) play() {
	defer t.dec.Close()
	var f video.Frame
	for u := range t.aus {
		held := time.Duration(0)
		due, sync, ok := t.r.syncDue(t, u.ts)
		if ok {
			if wait := time.Until(due); wait > 0 {
				time.Sleep(wait)
				held = wait
			}
		}
		decStart := time.Now()
		ok, err := t.dec.Decode(u.data, &f)
		decTook := time.Since(decStart)
		if err != nil {
			t.mu.Lock()
			t.st.DecodeErrs++
			t.waitKey = true
			if t.lostAt.IsZero() {
				t.lostAt = time.Now()
			}
			t.mu.Unlock()
			t.wantKey.Store(true)
			t.askKey()
			continue
		}
		if !ok {
			continue
		}
		t.mu.Lock()
		if d := int32(u.ts - t.shownTS); t.shown && d <= 0 && d >= -staleMost {
			t.st.Stale++
			t.mu.Unlock()
			continue
		}
		t.shown, t.shownTS = true, u.ts
		t.mu.Unlock()
		now := time.Now()
		f.At, f.Mirror = now, false
		t.latest.Put(f)
		t.mu.Lock()
		first := t.st.Frames == 0
		t.st.Frames++
		if u.key {
			t.st.Keyframes++
			if !t.lostAt.IsZero() {
				t.st.Recoveries = append(t.st.Recoveries, now.Sub(t.lostAt))
				t.lostAt = time.Time{}
			}
		}
		t.st.W, t.st.H = f.W, f.H
		t.holdSum += held
		t.holdN++
		if first {
			t.st.FirstFrame = now.Sub(t.subAt)
		}
		resized := t.lastSize != [2]int{f.W, f.H}
		t.lastSize = [2]int{f.W, f.H}
		t.mu.Unlock()
		if first || resized {
			slog.Info("video: frame", "identity", t.who, "kind", t.kind, "w", f.W, "h", f.H, "first", first, "sinceSubscribed", now.Sub(t.subAt).Round(time.Millisecond))
		}
		if hook := t.r.cfg.VideoFrame; hook != nil {
			hook(t, f, VideoFrameInfo{RTP: u.ts, Received: u.last, Assembled: u.at, Held: held, Sync: sync, Decode: decTook, Key: u.key})
		}
	}
}

// VideoFrameInfo is what a tool hears about each frame shown.
type VideoFrameInfo struct {
	RTP       uint32        // its RTP timestamp
	Received  time.Time     // when its last packet came
	Assembled time.Time     // when it was put together (after any hole before it)
	Held      time.Duration // how long lip sync held it
	Sync      SyncInfo      // how it was timed against the voice
	Decode    time.Duration // how long it took to decode
	Key       bool
}

// VideoUnitInfo is what a trace hears about each access unit put together.
type VideoUnitInfo struct {
	RTP     uint32
	Key     bool
	Gap     bool      // units were lost before it
	Skipped bool      // dropped: the decoder waits for a keyframe
	Lost    time.Time // with Gap: when the loss was found (and a keyframe asked for)
	Last    time.Time // when its last packet came
	At      time.Time // when it was put together
}

// Stats are the track's numbers so far.
func (t *videoTrack) Stats() VideoTrackStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.st
	s.Recoveries = append([]time.Duration(nil), t.st.Recoveries...)
	if t.holdN > 0 {
		s.Held = t.holdSum / time.Duration(t.holdN)
	}
	return s
}

func (t *videoTrack) logStats() {
	s := t.Stats()
	slog.Info("video: track", "identity", s.Identity, "kind", s.Kind, "w", s.W, "h", s.H, "frames", s.Frames, "keyframes", s.Keyframes,
		"units", s.Units, "skipped", s.Skipped, "gaps", s.Gaps, "plis", s.PLIs, "decodeErrs", s.DecodeErrs, "lostPkts", s.LostPkts,
		"cut", s.Cut, "stale", s.Stale, "lipSyncHold", s.Held.Round(time.Millisecond), "firstFrame", s.FirstFrame.Round(time.Millisecond))
}

// VideoTracks returns each remote video's numbers (for tools).
func VideoTracks(rm rtc.Room) []VideoTrackStats {
	r, ok := rm.(*room)
	if !ok {
		return nil
	}
	r.mu.Lock()
	ts := make([]*videoTrack, 0, len(r.videos))
	for _, t := range r.videos {
		ts = append(ts, t)
	}
	r.mu.Unlock()
	var out []VideoTrackStats
	for _, t := range ts {
		out = append(out, t.Stats())
	}
	return out
}

// srClock maps a track's RTP timestamps to a clock: the receiver's, from
// when its packets came (arrived: when each timestamp would have come with
// the least delay seen lately); and the sender's wall clock (NTP), from its
// sender reports. A person's voice and video compare on either clock, both
// by arrivals or both by reports: their packets cross the same network, so
// the least delay is the same for both.
//
// Lip sync goes by arrivals. They're there from the first packets, where
// reports come through LiveKit only seconds in (frames led the voice by
// 40–55 ms until then); and they're steadier: a sender report (pion's)
// stamps the last packet written, so a video's maps its frame's capture
// time to when the frame came out of the encoder, 1–13 ms later from one
// report to the next (one every 3 s through LiveKit; a frame after a
// keyframe more), where the least delay over a window takes the quickest.
// The voice's reports and arrivals agreed within a millisecond.
type srClock struct {
	mu  sync.Mutex
	ok  bool
	rtp uint32
	ntp time.Duration // since 1900

	// The arrival clock: timestamps unwrapped (ext, the last one's), and
	// the earliest arrival seen less the time its timestamp stands for,
	// in this window and the one before.
	arrOK     bool
	rate      float64
	last      uint32
	ext       int64
	win       time.Time
	cur, prev time.Time
	prevOK    bool
}

// arrWindow is how long the arrival clock's least delay is kept: long
// enough that a few packets come with none of the network's jitter, short
// enough to forget a timestamp that jumped, or clocks that drifted.
const arrWindow = 2 * time.Second

// arrived notes that the packet at ts came at at.
func (c *srClock) arrived(ts uint32, at time.Time, rate float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.arrOK {
		c.arrOK, c.rate, c.last, c.ext = true, rate, ts, int64(ts)
		c.win, c.cur = at, at.Add(-c.span(c.ext))
		return
	}
	c.ext += int64(int32(ts - c.last))
	c.last = ts
	base := at.Add(-c.span(c.ext))
	if at.Sub(c.win) >= arrWindow {
		c.prev, c.prevOK = c.cur, true
		c.win, c.cur = at, base
	} else if base.Before(c.cur) {
		c.cur = base
	}
}

// span is how long ext timestamp units are.
func (c *srClock) span(ext int64) time.Duration {
	return time.Duration(float64(ext) / c.rate * float64(time.Second))
}

// arrivedAt is when, on the receiver's clock, the sample at ts would have
// come with the least delay seen.
func (c *srClock) arrivedAt(ts uint32) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.arrOK {
		return time.Time{}, false
	}
	base := c.cur
	if c.prevOK && c.prev.Before(base) {
		base = c.prev
	}
	return base.Add(c.span(c.ext + int64(int32(ts-c.last)))), true
}

// read is the publication's RTCP: sender reports set the mapping.
func (c *srClock) read(p rtcp.Packet) {
	sr, ok := p.(*rtcp.SenderReport)
	if !ok {
		return
	}
	c.mu.Lock()
	c.ok, c.rtp, c.ntp = true, sr.RTPTime, ntpDuration(sr.NTPTime)
	c.mu.Unlock()
}

// ntpDuration is a 64-bit NTP timestamp as time since 1900.
func ntpDuration(ntp uint64) time.Duration {
	sec, frac := ntp>>32, ntp&0xffffffff
	return time.Duration(sec)*time.Second + time.Duration(frac*uint64(time.Second)>>32)
}

// at is when, on the sender's clock, the sample at ts was taken.
func (c *srClock) at(ts uint32, rate float64) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ok {
		return 0, false
	}
	d := float64(int32(ts-c.rtp)) / rate
	return c.ntp + time.Duration(d*float64(time.Second)), true
}

// voiceSent is how long after its first sample a voice packet goes out: a
// whole 20 ms Opus frame is captured first. Both lip-sync clocks place a
// voice timestamp that much late (sender reports are stamped as packets
// are written; arrivals come after the writes), where a video timestamp
// is its frame's capture time, placed late only by the encoding (a few
// ms, about what Opus's own delay puts the voice behind: they cancel).
// Without it every frame showed about 18 ms before its sound.
const voiceSent = 20 * time.Millisecond

// SyncInfo is how a video frame was timed against its sender's voice.
type SyncInfo struct {
	// How: "arrivals" (packets' arrival), "reports" (sender reports), or
	// empty: shown as decoded (no voice playing, nothing to compare by, or
	// due further off than maxHold).
	How string
	// How long after the voice's playing position the frame was taken, by
	// each clock (where there is one).
	Reports, Arrivals     time.Duration
	ReportsOK, ArrivalsOK bool
}

// syncDue is when a video frame of t at RTP timestamp ts should be shown so
// that it meets the same person's audio as it's heard; false when there's
// nothing to sync with (no audio playing, no clock both tracks have, or
// clocks too far apart).
func (r *room) syncDue(t *videoTrack, ts uint32) (time.Time, SyncInfo, bool) {
	var si SyncInfo
	a := r.audioOf(t.who)
	if a == nil {
		return time.Time{}, si, false
	}
	pts, pulled, ok := a.buf.Playing()
	if !ok || time.Since(pulled) > audioStale {
		return time.Time{}, si, false
	}
	gap, si, ok := syncGap(&t.sr, &a.sr, ts, pts)
	if !ok {
		return time.Time{}, si, false
	}
	// The audio at pts is heard at pulled plus the speaker's delay; the
	// frame is due as far after that as it was taken after pts.
	due := pulled.Add(r.speakerDelay() + gap)
	if d := time.Until(due); d > maxHold {
		si.How = ""
		return time.Time{}, si, false
	}
	return due, si, true
}

// syncGap is how long after the voice's sample at ats the video frame at
// vts was taken: by arrivals, else (no packets of one yet) by sender
// reports; SyncInfo has both. Either clock places a voice timestamp a
// packet late (voiceSent), which the gap takes off.
func syncGap(v, a *srClock, vts, ats uint32) (time.Duration, SyncInfo, bool) {
	var si SyncInfo
	if vAt, ok := v.at(vts, 90000); ok {
		if aAt, ok := a.at(ats, 48000); ok {
			si.Reports, si.ReportsOK = vAt-aAt+voiceSent, true
		}
	}
	if vAt, ok := v.arrivedAt(vts); ok {
		if aAt, ok := a.arrivedAt(ats); ok {
			si.Arrivals, si.ArrivalsOK = vAt.Sub(aAt)+voiceSent, true
		}
	}
	switch {
	case si.ArrivalsOK:
		si.How = "arrivals"
		return si.Arrivals, si, true
	case si.ReportsOK:
		si.How = "reports"
		return si.Reports, si, true
	}
	return 0, si, false
}

// audioOf is a participant's audio track (its first).
func (r *room) audioOf(who string) *remote {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.tracks {
		if a.who == who {
			return a
		}
	}
	return nil
}

// speakerDelay is how long after the mix pulls audio it is heard.
func (r *room) speakerDelay() time.Duration {
	r.mu.Lock()
	spk := r.spk
	r.mu.Unlock()
	if l, ok := spk.(interface{ Latency() time.Duration }); ok {
		return l.Latency()
	}
	return 0
}
