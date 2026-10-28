package sfu

// The harness for the SFU's tests: an in-process Server behind httptest on
// loopback, and sfuclient sessions that record everything they hear.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// wait is how long a test waits for something that should happen at once:
// generous, since other builds may share the machine.
const wait = 10 * time.Second

// testLog sends a logger's lines to t.Log while the test runs (shown with
// -v or on failure), and drops them after.
func testLog(t *testing.T) *slog.Logger {
	h := &tlogHandler{t: t, mu: &sync.Mutex{}, done: new(bool)}
	t.Cleanup(func() {
		h.mu.Lock()
		*h.done = true
		h.mu.Unlock()
	})
	return slog.New(h)
}

type tlogHandler struct {
	t     *testing.T
	mu    *sync.Mutex
	done  *bool
	attrs []slog.Attr
}

func (h *tlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *tlogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", r.Time.Format("15:04:05.000"), r.Level, r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if !*h.done {
		h.t.Log(b.String())
	}
	return nil
}

func (h *tlogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(slices.Clone(h.attrs), as...)
	return &c
}

func (h *tlogHandler) WithGroup(string) slog.Handler { return h }

// testSFU is an SFU on loopback with its /rtc behind httptest.
type testSFU struct {
	s      *Server
	url    string
	tokens rtctoken.Minter
	log    *slog.Logger
}

// newTestSFU starts one; tune, when set, changes the server before it
// takes any request.
func newTestSFU(t *testing.T, tune func(*Server)) *testSFU {
	t.Helper()
	log := testLog(t)
	tokens := rtctoken.Minter{Secret: []byte("a test secret")}
	s, err := New(Config{Network: Network{IP: "127.0.0.1", Loopback: true}, Tokens: tokens, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if tune != nil {
		tune(s)
	}
	mux := http.NewServeMux()
	mux.Handle("/rtc", s)
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		s.Close()
		hs.Close()
	})
	return &testSFU{s: s, url: "ws" + strings.TrimPrefix(hs.URL, "http") + "/rtc", tokens: tokens, log: log}
}

func (ts *testSFU) token(t *testing.T, room, identity string) string {
	t.Helper()
	tok, err := ts.tokens.Mint(rtctoken.Grant{Room: room, Identity: identity, Name: strings.ToUpper(identity)})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// heard is what a client heard of one remote track.
type heard struct {
	sid, from string
	kind      signal.TrackKind
	rt        *sfuclient.RemoteTrack
	addedAt   time.Time
	firstAt   time.Time
	frames    []int    // frame numbers, in the order they came
	tss       []uint32 // each one's RTP timestamp
	levels    []byte   // each one's RFC 6464 byte (0xff: none)
	bad       []string // packets that weren't a frame the publisher wrote
	packets   int
	ended     bool // the track's reads ended: it came off the connection
	srs       []srSeen
}

// srSeen is a sender report as heard (or sent), and when.
type srSeen struct {
	ntp uint64
	rtp uint32
	at  time.Time
}

// testClient is a session in a test, recording everything it hears.
type testClient struct {
	t  *testing.T
	id string
	s  *sfuclient.Session

	mu           sync.Mutex
	events       []string // joined, arrived, left, info, added, removed: in order
	joined       []string // others, as Joined listed them
	tracks       map[string]*heard
	speakers     [][]signal.Speaker
	subConnected time.Time
	disconnected error
}

// join has identity join room with opts.
func (ts *testSFU) join(t *testing.T, room, identity string, opts sfuclient.Options) *testClient {
	t.Helper()
	c := &testClient{t: t, id: identity, tracks: map[string]*heard{}}
	opts.Loopback = true
	if opts.Log == nil {
		opts.Log = ts.log
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	s, err := sfuclient.Join(ctx, ts.url, ts.token(t, room, identity), opts, c.handlers())
	if err != nil {
		t.Fatalf("%s: %v", identity, err)
	}
	c.s = s
	t.Cleanup(func() { s.Close() })
	return c
}

func (c *testClient) record(format string, args ...any) {
	c.events = append(c.events, fmt.Sprintf(format, args...))
}

func (c *testClient) handlers() sfuclient.Events {
	return sfuclient.Events{
		Joined: func(self signal.Participant, others []signal.Participant) {
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, o := range others {
				c.joined = append(c.joined, o.Identity)
			}
			c.record("joined %s", strings.Join(c.joined, ","))
		},
		Arrived: func(p signal.Participant) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.record("arrived %s", p.Identity)
		},
		Left: func(id string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.record("left %s", id)
		},
		TrackInfo: func(ti signal.TrackInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.record("info %s %s muted=%v", ti.Participant, ti.Kind, ti.Muted)
		},
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			c.mu.Lock()
			h := &heard{sid: rt.SID, from: rt.Participant, kind: rt.Kind, rt: rt, addedAt: time.Now()}
			c.tracks[rt.SID] = h
			c.record("added %s %s", rt.Participant, rt.Kind)
			c.mu.Unlock()
			go c.read(h)
			go c.readReports(h)
		},
		TrackRemoved: func(rt *sfuclient.RemoteTrack) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.record("removed %s %s", rt.Participant, rt.Kind)
		},
		Speakers: func(sp []signal.Speaker) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.speakers = append(c.speakers, sp)
		},
		Connection: func(pc signal.PC, st webrtc.PeerConnectionState) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if pc == signal.Sub && st == webrtc.PeerConnectionStateConnected && c.subConnected.IsZero() {
				c.subConnected = time.Now()
			}
		},
		Disconnected: func(err error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.disconnected = err
			c.record("disconnected")
		},
	}
}

// read records every packet of a track until it ends. A voice's packets
// must each be a frame its publisher wrote (frameBytes).
func (c *testClient) read(h *heard) {
	var levelID uint8
	for _, e := range h.rt.Receiver.GetParameters().HeaderExtensions {
		if e.URI == sdp.AudioLevelURI {
			levelID = uint8(e.ID)
		}
	}
	for {
		p, _, err := h.rt.Track.ReadRTP()
		now := time.Now()
		c.mu.Lock()
		if err != nil {
			h.ended = true
			c.mu.Unlock()
			return
		}
		h.packets++
		if h.firstAt.IsZero() {
			h.firstAt = now
		}
		if h.kind == signal.Audio {
			who, n, ok := parseFrame(p.Payload)
			switch {
			case !ok || who != h.from:
				h.bad = append(h.bad, fmt.Sprintf("payload %q", p.Payload))
			case p.Padding || p.PaddingSize > 0:
				h.bad = append(h.bad, fmt.Sprintf("frame %d padded", n))
			default:
				h.frames = append(h.frames, n)
				h.tss = append(h.tss, p.Timestamp)
				lv := byte(0xff)
				if b := p.GetExtension(levelID); levelID != 0 && len(b) > 0 {
					lv = b[0]
				}
				h.levels = append(h.levels, lv)
			}
		}
		c.mu.Unlock()
	}
}

// readReports records the track's sender reports.
func (c *testClient) readReports(h *heard) {
	for {
		pkts, _, err := h.rt.Receiver.ReadRTCP()
		if err != nil {
			return
		}
		now := time.Now()
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok && sr.SSRC == uint32(h.rt.Track.SSRC()) {
				c.mu.Lock()
				h.srs = append(h.srs, srSeen{ntp: sr.NTPTime, rtp: sr.RTPTime, at: now})
				c.mu.Unlock()
			}
		}
	}
}

// from is the track c hears of who's, of kind (nil if none yet).
func (c *testClient) from(who string, kind signal.TrackKind) *heard {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fromLocked(who, kind)
}

func (c *testClient) fromLocked(who string, kind signal.TrackKind) *heard {
	var last *heard
	for _, h := range c.tracks {
		if h.from == who && h.kind == kind && (last == nil || h.addedAt.After(last.addedAt)) {
			last = h
		}
	}
	return last
}

// framesFrom is a copy of the frames c has heard from who's voice.
func (c *testClient) framesFrom(who string) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h := c.fromLocked(who, signal.Audio); h != nil {
		return slices.Clone(h.frames)
	}
	return nil
}

func (c *testClient) eventLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// has says whether c's events include e.
func (c *testClient) has(e string) bool { return slices.Contains(c.eventLog(), e) }

// waitFor polls cond until it holds, or fails the test after d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// frameBytes is frame n of who's voice: a stand-in for an Opus packet that
// says whose it is and which.
func frameBytes(who string, n int) []byte { return []byte(who + "#" + strconv.Itoa(n)) }

func parseFrame(b []byte) (string, int, bool) {
	who, num, ok := strings.Cut(string(b), "#")
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	return who, n, err == nil
}

// speaker writes a voice in real time: frame n at its start plus n × 20 ms,
// RTP timestamp base + n × 960, at the level it's set to. Frames the track
// isn't negotiated for yet aren't counted: frame 0 is the first written.
// A clocked speaker stamps each frame with when it's written instead, as a
// capture clock would, so a timer that wakes late doesn't move the RTP
// time its sender reports give.
type speaker struct {
	who     string
	track   *sfuclient.Track
	base    uint32
	clocked bool
	start   time.Time // frame 0's moment (and, clocked, RTP time base)
	level   atomic.Uint32
	sent    atomic.Int64 // frames written
	stop    chan struct{}
	done    chan struct{}

	mu  sync.Mutex
	tss []uint32 // each frame's RTP timestamp
}

func speak(t *testing.T, who string, track *sfuclient.Track, level uint8, clocked bool) *speaker {
	sp := &speaker{who: who, track: track, base: uint32(len(who)) * 1_000_003, clocked: clocked, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	sp.level.Store(uint32(level))
	go sp.run()
	t.Cleanup(sp.halt)
	return sp
}

func (sp *speaker) run() {
	defer close(sp.done)
	start := sp.start
	n := 0
	for i := 0; ; i++ {
		select {
		case <-sp.stop:
			return
		case <-time.After(time.Until(start.Add(time.Duration(i) * 20 * time.Millisecond))):
		}
		lv := uint8(sp.level.Load())
		ts := sp.base + uint32(n)*960
		if sp.clocked {
			ts = sp.base + uint32(time.Since(start).Seconds()*48000)
		}
		if err := sp.track.WriteFrame(frameBytes(sp.who, n), ts, &lv); err == nil {
			sp.mu.Lock()
			sp.tss = append(sp.tss, ts)
			sp.mu.Unlock()
			n++
			sp.sent.Store(int64(n))
		}
	}
}

// halt stops the voice and waits for its last frame.
func (sp *speaker) halt() {
	select {
	case <-sp.stop:
	default:
		close(sp.stop)
	}
	<-sp.done
}

// tsOf is the RTP timestamp the speaker wrote frame n at.
func (sp *speaker) tsOf(n int) uint32 {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.tss[n]
}

// writtenAt is when a clocked speaker wrote frame n (its RTP time on the
// speaker's clock), if it has.
func (sp *speaker) writtenAt(n int) (time.Time, bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if !sp.clocked || n >= len(sp.tss) {
		return time.Time{}, false
	}
	return sp.start.Add(time.Duration(sp.tss[n]-sp.base) * time.Second / 48000), true
}

// What tests look at on the SFU's side.

func (r *room) participant(identity string) *participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parts[identity]
}

func (p *participant) publication(sid string) *publication {
	r := p.rm.Load()
	r.mu.Lock()
	defer r.mu.Unlock()
	return p.pubs[sid]
}

// sending says whether a down track for sid is on the connection.
func (t *transport) sending(sid string) bool {
	t.op.Lock()
	defer t.op.Unlock()
	return t.senders[sid] != nil
}

// mlines is how many media sections the connection has.
func (t *transport) mlines() int {
	t.op.Lock()
	defer t.op.Unlock()
	return len(t.pc.GetTransceivers())
}

// checkRun fails unless frames (as heard) are first, first+1, … last with
// nothing missing, repeated or out of order, and the run began by from.
func checkRun(t *testing.T, what string, frames []int, from, last int) {
	t.Helper()
	if len(frames) == 0 {
		t.Errorf("%s: heard nothing", what)
		return
	}
	first := frames[0]
	for i, n := range frames {
		if n != first+i {
			t.Errorf("%s: frame %d where %d was due (of %d heard from %d)", what, n, first+i, len(frames), first)
			return
		}
	}
	if first > from {
		t.Errorf("%s: heard from frame %d, after %d", what, first, from)
	}
	if got := frames[len(frames)-1]; got != last {
		t.Errorf("%s: last frame heard %d, want %d", what, got, last)
	}
}
