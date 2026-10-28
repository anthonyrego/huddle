package sfu

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// localHears records what an in-process participant hears.
type localHears struct {
	mu     sync.Mutex
	events []string
	voices map[string]*localVoice
}

// localVoice is one voice handed over, read until it ends.
type localVoice struct {
	who    string
	v      *Voice
	frames []int
	seqs   []uint16
	got    []time.Time // when each frame was read
	bad    []string
	err    error // how the reads ended
}

func newLocalHears() *localHears { return &localHears{voices: map[string]*localVoice{}} }

func (h *localHears) events_() LocalEvents {
	return LocalEvents{
		Arrived: func(id, name string) { h.add("arrived " + id + " " + name) },
		Left:    func(id string) { h.add("left " + id) },
		Voice: func(id, _ string, v *Voice) {
			h.add("voice " + id)
			lv := &localVoice{who: id, v: v}
			h.mu.Lock()
			h.voices[id] = lv
			h.mu.Unlock()
			go h.read(lv)
		},
	}
}

func (h *localHears) add(e string) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *localHears) read(lv *localVoice) {
	for {
		lv.v.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		p, _, err := lv.v.ReadRTP()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			continue
		}
		h.mu.Lock()
		if err != nil {
			lv.err = err
			h.mu.Unlock()
			return
		}
		who, n, ok := parseFrame(p.Payload)
		if !ok || who != lv.who || p.SSRC == 0 || p.PayloadType != codecs.OpusPT {
			lv.bad = append(lv.bad, string(p.Payload))
		} else {
			lv.frames = append(lv.frames, n)
			lv.seqs = append(lv.seqs, p.SequenceNumber)
			lv.got = append(lv.got, time.Now())
		}
		h.mu.Unlock()
	}
}

func (h *localHears) log() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *localHears) voice(who string) *localVoice {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.voices[who]
}

func (h *localHears) frames(who string) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lv := h.voices[who]; lv != nil {
		return slices.Clone(lv.frames)
	}
	return nil
}

func (h *localHears) ended(who string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lv := h.voices[who]; lv != nil {
		return lv.err
	}
	return nil
}

// TestLocal is the in-process participant in a room: it hears the people
// (those there at its join and those who come, never an agent), is handed
// each person's voice and never a camera or an agent's voice, publishes a
// voice everyone hears with translated sender reports, counts the people,
// and each voice ends when its publisher unpublishes or leaves.
func TestLocal(t *testing.T) {
	ts := newTestSFU(t, nil)
	ann := ts.join(t, "r", "ann", sfuclient.Options{})
	annVoice, _ := publishSpeaker(t, ann, 30, true)

	// Ann's camera, its three layers written as she talks.
	var layers []*sfuclient.Track
	for _, rid := range []string{"q", "h", "f"} {
		tr, err := sfuclient.NewTrack(codecs.H264, "cam", "ann", rid, nil)
		if err != nil {
			t.Fatal(err)
		}
		layers = append(layers, tr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, err := ann.s.Publish(ctx, signal.Publish{CID: "cam", Kind: signal.Video, Source: signal.Camera,
		Layers: []signal.Layer{{RID: "f", W: 1280, H: 720}, {RID: "h", W: 640, H: 360}, {RID: "q", W: 320, H: 180}}}, layers...); err != nil {
		t.Fatal(err)
	}
	camStop := make(chan struct{})
	camDone := make(chan struct{})
	go func() {
		defer close(camDone)
		for ts := uint32(0); ; ts += 3000 {
			select {
			case <-camStop:
				return
			case <-time.After(33 * time.Millisecond):
			}
			for _, l := range layers {
				l.WriteFrame(frame, ts, nil)
			}
		}
	}()
	defer func() { close(camStop); <-camDone }()

	// Another agent, with a voice of its own: never heard by the local one.
	tok, err := ts.tokens.Mint(rtctoken.Grant{Room: "r", Identity: "scribe", Name: "Scribe", Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	scribe := &testClient{t: t, id: "scribe", tracks: map[string]*heard{}}
	if scribe.s, err = sfuclient.Join(ctx, ts.url, tok, sfuclient.Options{Loopback: true, Log: ts.log}, scribe.handlers()); err != nil {
		t.Fatal(err)
	}
	defer scribe.s.Close()
	publishSpeaker(t, scribe, 30, false)
	waitFor(t, wait, "ann to hear the scribe", func() bool { return len(ann.framesFrom("scribe")) > 0 })

	// The whiteboard joins, in-process.
	h := newLocalHears()
	wb, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "whiteboard", Name: "Whiteboard", Agent: true}, h.events_())
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Leave()
	waitFor(t, wait, "the whiteboard to hear ann", func() bool { return len(h.frames("ann")) >= 25 })
	if got := wb.People(); got != 1 {
		t.Errorf("People: %d, want 1 (ann; the scribe is an agent)", got)
	}
	for _, x := range []*testClient{ann, scribe} {
		waitFor(t, wait, x.id+" to hear the whiteboard arrive", func() bool { return x.has("arrived whiteboard") })
	}

	// Bea comes after, with a voice.
	bea := ts.join(t, "r", "bea", sfuclient.Options{})
	beaVoice, beaSID := publishVoice(t, bea, 40)
	waitFor(t, wait, "the whiteboard to hear bea", func() bool { return len(h.frames("bea")) >= 10 })
	if got := wb.People(); got != 2 {
		t.Errorf("People: %d, want 2", got)
	}

	// The whiteboard speaks, at RTP times that follow its clock.
	track, _, err := wb.PublishAudio("whiteboard voice")
	if err != nil {
		t.Fatal(err)
	}
	ts0 := uint32(7_000_000)
	start := time.Now()
	wbStop, wbDone := make(chan struct{}), make(chan struct{})
	var wbSent int
	go func() {
		defer close(wbDone)
		for n := 0; ; n++ {
			select {
			case <-wbStop:
				return
			case <-time.After(time.Until(start.Add(time.Duration(n) * 20 * time.Millisecond))):
			}
			if track.WriteFrame(frameBytes("whiteboard", n), ts0+uint32(time.Since(start).Seconds()*48000), nil) == nil {
				wbSent = n + 1
			}
		}
	}()
	for _, x := range []*testClient{ann, bea} {
		waitFor(t, wait, x.id+" to hear the whiteboard", func() bool { return len(x.framesFrom("whiteboard")) >= 75 })
	}
	close(wbStop)
	<-wbDone
	time.Sleep(200 * time.Millisecond)
	for _, x := range []*testClient{ann, bea} {
		// It writes the moment it has published, unlike a remote publisher,
		// whose own connection takes longer than the others' offers: the
		// frames before each down connection carries the track aren't
		// heard (no buffering, as for anyone).
		checkRun(t, x.id+" hearing the whiteboard", x.framesFrom("whiteboard"), 10, wbSent-1)
		x.mu.Lock()
		hd := x.fromLocked("whiteboard", signal.Audio)
		if len(hd.bad) > 0 {
			t.Errorf("%s: packets not the whiteboard's: %q", x.id, hd.bad)
		}
		// Its sender reports: RTP time against the SFU's clock, as the
		// whiteboard stamped it (within its 20 ms ticks).
		if len(hd.srs) == 0 {
			t.Errorf("%s: no sender report for the whiteboard's voice", x.id)
		}
		for _, sr := range hd.srs {
			at := ntpTime(sr.ntp)
			want := ts0 + uint32(at.Sub(start).Seconds()*48000)
			if d := int32(sr.rtp - want); d < -48*25 || d > 48*25 {
				t.Errorf("%s: a sender report %d ms off the whiteboard's clock", x.id, d/48)
			}
		}
		t.Logf("%s heard %d of the whiteboard's %d frames, from frame %d, and %d sender reports", x.id, len(hd.frames), wbSent, hd.frames[0], len(hd.srs))
		x.mu.Unlock()
	}

	// Bea unpublishes: her voice ends. Ann leaves: hers does, and she's gone.
	beaVoice.halt()
	if err := bea.s.Unpublish(beaSID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, wait, "bea's voice to end", func() bool { return h.ended("bea") != nil })
	annVoice.halt()
	ann.s.Close()
	waitFor(t, wait, "ann's voice to end", func() bool { return h.ended("ann") != nil })
	waitFor(t, wait, "the whiteboard to hear ann leave", func() bool { return slices.Contains(h.log(), "left ann") })
	if err := h.ended("ann"); err != io.EOF {
		t.Errorf("ann's voice ended with %v, want io.EOF", err)
	}
	if got := wb.People(); got != 1 {
		t.Errorf("People after ann left: %d, want 1", got)
	}

	want := []string{"arrived ann ANN", "voice ann", "arrived bea BEA", "voice bea", "left ann"}
	if got := h.log(); !slices.Equal(got, want) {
		t.Errorf("the whiteboard heard %q, want %q", got, want)
	}
	for _, who := range []string{"ann", "bea"} {
		lv := h.voice(who)
		h.mu.Lock()
		if len(lv.bad) > 0 {
			t.Errorf("%s's voice: packets not hers: %q", who, lv.bad)
		}
		for i := 1; i < len(lv.frames); i++ {
			if lv.frames[i] != lv.frames[i-1]+1 || lv.seqs[i] != lv.seqs[i-1]+1 {
				t.Errorf("%s's voice: frame %d (seq %d) after %d (seq %d)", who, lv.frames[i], lv.seqs[i], lv.frames[i-1], lv.seqs[i-1])
				break
			}
		}
		h.mu.Unlock()
	}

	// The transit, from ann's write to the whiteboard's read: ann's frame n
	// was written at its clocked timestamp.
	lv := h.voice("ann")
	h.mu.Lock()
	var transit []time.Duration
	for i, n := range lv.frames {
		if written, ok := annVoice.writtenAt(n); ok {
			transit = append(transit, lv.got[i].Sub(written))
		}
	}
	h.mu.Unlock()
	if len(transit) > 0 {
		sort.Slice(transit, func(i, j int) bool { return transit[i] < transit[j] })
		t.Logf("ann's write → the whiteboard's read: median %v, p95 %v over %d frames",
			transit[len(transit)/2].Round(10*time.Microsecond), transit[len(transit)*95/100].Round(10*time.Microsecond), len(transit))
	}

	// The whiteboard leaves: bea hears it go, and its voice come off.
	wb.Leave()
	waitFor(t, wait, "bea to hear the whiteboard leave", func() bool { return bea.has("left whiteboard") })
	if err := track.WriteFrame(frameBytes("whiteboard", 0), 0, nil); !errors.Is(err, ErrUnpublished) {
		t.Errorf("a write after leaving: %v", err)
	}
	if got := wb.People(); got != 0 {
		t.Errorf("People after leaving: %d", got)
	}
}

// ntpTime is a 64-bit NTP timestamp as a time.
func ntpTime(ntp uint64) time.Time {
	sec := int64(ntp>>32) - ntpEpoch
	frac := int64((ntp & 0xffffffff) * 1e9 >> 32)
	return time.Unix(sec, frac)
}

// A voice nobody reads holds the room up for no one: the others hear every
// frame, and the voice keeps the newest.
func TestLocalSlowReader(t *testing.T) {
	ts := newTestSFU(t, nil)
	ann := ts.join(t, "r", "ann", sfuclient.Options{})
	bea := ts.join(t, "r", "bea", sfuclient.Options{})
	var handed []*Voice
	var mu sync.Mutex
	wb, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "whiteboard", Agent: true}, LocalEvents{
		Voice: func(_, _ string, v *Voice) { mu.Lock(); handed = append(handed, v); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Leave()
	sp, _ := publishVoice(t, ann, 30)
	hearsAll(t, []*testClient{bea}, []*speaker{sp})
	readyAt := int(sp.sent.Load())
	time.Sleep(1500 * time.Millisecond)
	sent := allHeard(t, []*testClient{bea}, []*speaker{sp})
	checkRun(t, "bea hearing ann beside a voice nobody reads", bea.framesFrom("ann"), readyAt, sent["ann"]-1)
	mu.Lock()
	defer mu.Unlock()
	if len(handed) != 1 {
		t.Fatalf("%d voices handed over", len(handed))
	}
	v := handed[0]
	v.mu.Lock()
	queued := v.n
	v.mu.Unlock()
	if queued < sent["ann"]-5 || v.Dropped() != 0 {
		t.Errorf("the unread voice holds %d of %d frames, %d dropped", queued, sent["ann"], v.Dropped())
	}
}

// A Voice's queue: the oldest dropped past voiceQueue, the read deadline a
// net.Error that times out, what's queued read after the end, then io.EOF.
func TestVoiceQueue(t *testing.T) {
	v := newVoice(codecs.Opus)
	for i := range voiceQueue + 50 {
		v.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}})
	}
	if v.Dropped() != 50 {
		t.Errorf("dropped %d, want 50", v.Dropped())
	}
	p, _, err := v.ReadRTP()
	if err != nil || p.SequenceNumber != 50 {
		t.Fatalf("first read: %v, %v; want the 51st packet", p, err)
	}
	for range voiceQueue - 1 {
		if _, _, err := v.ReadRTP(); err != nil {
			t.Fatal(err)
		}
	}
	v.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	start := time.Now()
	_, _, err = v.ReadRTP()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("an empty read past the deadline: %v", err)
	}
	if d := time.Since(start); d < 25*time.Millisecond || d > time.Second {
		t.Errorf("the deadline came after %v", d)
	}
	// A packet arriving while a read waits wakes it.
	v.SetReadDeadline(time.Time{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		v.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9}})
		v.close()
	}()
	if p, _, err := v.ReadRTP(); err != nil || p.SequenceNumber != 9 {
		t.Fatalf("a read woken by a packet: %v, %v", p, err)
	}
	if _, _, err := v.ReadRTP(); err != io.EOF {
		t.Fatalf("after the end: %v", err)
	}
	if c := v.Codec(); c.MimeType != codecs.Opus.MimeType || c.PayloadType != codecs.OpusPT {
		t.Errorf("codec %+v", c)
	}
}

// The local participant goes when a session joins under its identity, and
// when the server closes; either way its voices end.
func TestLocalEnds(t *testing.T) {
	ts := newTestSFU(t, nil)
	ann := ts.join(t, "r", "ann", sfuclient.Options{})
	publishVoice(t, ann, 30)
	h := newLocalHears()
	if _, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "whiteboard", Agent: true}, h.events_()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, wait, "the whiteboard to hear ann", func() bool { return len(h.frames("ann")) > 0 })
	ts.join(t, "r", "whiteboard", sfuclient.Options{})
	waitFor(t, wait, "the replaced whiteboard's voice to end", func() bool { return h.ended("ann") != nil })

	h2 := newLocalHears()
	if _, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "scribe", Agent: true}, h2.events_()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, wait, "the scribe to hear ann", func() bool { return len(h2.frames("ann")) > 0 })
	start := time.Now()
	ts.s.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the server took %v to close", d)
	}
	waitFor(t, wait, "the scribe's voice to end", func() bool { return h2.ended("ann") != nil })
	if _, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "late"}, LocalEvents{}); err == nil {
		t.Error("a local join on a closed server")
	}
}
