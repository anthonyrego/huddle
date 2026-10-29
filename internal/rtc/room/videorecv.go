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
		if due, ok := t.r.syncDue(t, u.ts); ok {
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
			hook(t, f, VideoFrameInfo{RTP: u.ts, Received: u.last, Assembled: u.at, Held: held, Decode: decTook, Key: u.key})
		}
	}
}

// VideoFrameInfo is what a tool hears about each frame shown.
type VideoFrameInfo struct {
	RTP       uint32        // its RTP timestamp
	Received  time.Time     // when its last packet came
	Assembled time.Time     // when it was put together (after any hole before it)
	Held      time.Duration // how long lip sync held it
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

// srClock maps a track's RTP timestamps to the sender's wall clock (NTP),
// from its sender reports.
type srClock struct {
	mu   sync.Mutex
	ok   bool
	rtp  uint32
	ntp  time.Duration // since 1900
	rate float64
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

// syncDue is when a video frame of t at RTP timestamp ts should be shown so
// that it meets the same person's audio as it's heard; false when there's
// nothing to sync with (no audio playing, no sender reports yet, or clocks
// too far apart).
func (r *room) syncDue(t *videoTrack, ts uint32) (time.Time, bool) {
	vAt, ok := t.sr.at(ts, 90000)
	if !ok {
		return time.Time{}, false
	}
	a := r.audioOf(t.who)
	if a == nil {
		return time.Time{}, false
	}
	pts, pulled, ok := a.buf.Playing()
	if !ok || time.Since(pulled) > audioStale {
		return time.Time{}, false
	}
	aAt, ok := a.sr.at(pts, 48000)
	if !ok {
		return time.Time{}, false
	}
	// The audio at pts is heard at pulled plus the speaker's delay; the
	// frame is due as far after that as it was taken after pts.
	due := pulled.Add(r.speakerDelay() + (vAt - aAt))
	if d := time.Until(due); d > maxHold {
		return time.Time{}, false
	}
	return due, true
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
