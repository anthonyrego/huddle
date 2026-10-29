package room

// The builtin transport against huddle's real SFU (internal/server/sfu),
// in process, behind httptest on loopback: no devices.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/fileio"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/server/sfu"
)

// sfuWait is how long a test waits for something that should happen at
// once: generous, since other builds may share the machine.
const sfuWait = 10 * time.Second

// testSFU is an SFU on loopback with its /rtc behind httptest.
type testSFU struct {
	url    string
	tokens rtctoken.Minter
}

func newTestSFU(t *testing.T) *testSFU {
	t.Helper()
	tokens := rtctoken.Minter{Secret: []byte("a test secret")}
	s, err := sfu.New(sfu.Config{Network: sfu.Network{IP: "127.0.0.1", Loopback: true}, Tokens: tokens, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/rtc", s)
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		s.Close()
		hs.Close()
	})
	return &testSFU{url: "ws" + strings.TrimPrefix(hs.URL, "http") + "/rtc", tokens: tokens}
}

// join is what the dev join would hand identity for room.
func (ts *testSFU) join(t *testing.T, room, identity string, agent bool) rtc.Join {
	t.Helper()
	tok, err := ts.tokens.Mint(rtctoken.Grant{Room: room, Identity: identity, Name: strings.ToUpper(identity), Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	return rtc.Join{URL: ts.url, Token: tok, Identity: identity}
}

// testLog sends a logger's lines to t.Log while the test runs, and drops
// them after.
func testLog(t *testing.T) *slog.Logger {
	h := &tlog{t: t, mu: &sync.Mutex{}, done: new(bool)}
	t.Cleanup(func() {
		h.mu.Lock()
		*h.done = true
		h.mu.Unlock()
	})
	return slog.New(h)
}

type tlog struct {
	t     *testing.T
	mu    *sync.Mutex
	done  *bool
	attrs []slog.Attr
}

func (h *tlog) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *tlog) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Time.Format("15:04:05.000"), r.Message)
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

func (h *tlog) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(slices.Clone(h.attrs), as...)
	return &c
}

func (h *tlog) WithGroup(string) slog.Handler { return h }

// waitUntil polls cond until it holds, or fails the test after d.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heardTrack is a track a recorder was sent, and what it read of it.
type heardTrack struct {
	rt      remoteTrack
	packets atomic.Int64
	levels  atomic.Int64 // packets with the audio level on
	srs     atomic.Int64 // sender reports through the room's hook
	ended   atomic.Bool
}

// recorder is a room's side of the transport, recording what it's told.
type recorder struct {
	mu       sync.Mutex
	events   []string
	arrived  map[string]rtc.Participant
	tracks   map[string]*heardTrack // by SID
	speakers []map[string]float32
	reason   string // disconnected
}

func newRecorder() *recorder {
	return &recorder{arrived: map[string]rtc.Participant{}, tracks: map[string]*heardTrack{}}
}

func (r *recorder) note(format string, args ...any) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) has(e string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.events, e)
}

func (r *recorder) eventLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *recorder) track(who string, kind webrtc.RTPCodecType) *heardTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.tracks {
		if h.rt.who == who && h.rt.kind == kind {
			return h
		}
	}
	return nil
}

// lastSpeakers is the newest speakers map told.
func (r *recorder) lastSpeakers() map[string]float32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.speakers) == 0 {
		return nil
	}
	return r.speakers[len(r.speakers)-1]
}

func (r *recorder) handlers() transportEvents {
	return transportEvents{
		arrived: func(p rtc.Participant) {
			r.mu.Lock()
			r.arrived[p.Identity] = p
			r.mu.Unlock()
			r.note("arrived %s", p.Identity)
		},
		left: func(id string) { r.note("left %s", id) },
		subscribed: func(rt remoteTrack) func(rtcp.Packet) {
			h := &heardTrack{rt: rt}
			r.mu.Lock()
			r.tracks[rt.sid] = h
			r.mu.Unlock()
			r.note("subscribed %s %s", rt.who, rt.kind)
			go func() {
				for {
					p, _, err := rt.track.ReadRTP()
					if err != nil {
						h.ended.Store(true)
						return
					}
					h.packets.Add(1)
					if rt.levelExt != 0 && len(p.GetExtension(uint8(rt.levelExt))) > 0 {
						h.levels.Add(1)
					}
				}
			}()
			return func(p rtcp.Packet) {
				if _, ok := p.(*rtcp.SenderReport); ok {
					h.srs.Add(1)
				}
			}
		},
		unsubscribed: func(sid string) {
			r.mu.Lock()
			h := r.tracks[sid]
			r.mu.Unlock()
			who := "?"
			if h != nil {
				who = h.rt.who + " " + h.rt.kind.String()
			}
			r.note("unsubscribed %s", who)
		},
		speakers: func(ls map[string]float32) {
			r.mu.Lock()
			r.speakers = append(r.speakers, ls)
			r.mu.Unlock()
		},
		reconnecting: func() { r.note("reconnecting") },
		reconnected:  func() { r.note("reconnected") },
		disconnected: func(reason string) {
			r.mu.Lock()
			r.reason = reason
			r.mu.Unlock()
			r.note("disconnected")
		},
	}
}

// dialTest joins room as identity on the builtin transport.
func dialTest(t *testing.T, ts *testSFU, room, identity string, agent bool, ev transportEvents) *sfuTransport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sfuWait)
	defer cancel()
	tr, err := dialSFU(ctx, ts.join(t, room, identity, agent), dialConfig{subscribe: true}, ev)
	if err != nil {
		t.Fatalf("%s: %v", identity, err)
	}
	t.Cleanup(tr.leave)
	return tr.(*sfuTransport)
}

// voice writes stand-in frames to a published mic every 20 ms at a level,
// until stopped.
type voice struct {
	out   audioOut
	level atomic.Int32
	stop  chan struct{}
	done  chan struct{}
}

func speakOn(out audioOut, level uint8) *voice {
	v := &voice{out: out, stop: make(chan struct{}), done: make(chan struct{})}
	v.level.Store(int32(level))
	go func() {
		defer close(v.done)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for ts := uint32(1); ; ts += 960 {
			select {
			case <-v.stop:
				return
			case <-tk.C:
			}
			v.out.write([]byte("a stand-in frame"), ts, uint8(v.level.Load()))
		}
	}()
	return v
}

func (v *voice) halt() {
	select {
	case <-v.stop:
	default:
		close(v.stop)
	}
	<-v.done
}

// TestSFUTransport is the transport's mapping of sfuclient: participants
// (an agent among them), a voice subscribed with its codec, level and
// sender reports, speakers, a mute, a simulcast camera whose every layer
// reaches the SFU, the estimator, people leaving, and the server's bye.
func TestSFUTransport(t *testing.T) {
	ts := newTestSFU(t)
	ann, bob, wb := newRecorder(), newRecorder(), newRecorder()
	a := dialTest(t, ts, "r", "ann", false, ann.handlers())
	b := dialTest(t, ts, "r", "bob", false, bob.handlers())
	w := dialTest(t, ts, "r", "wb", true, wb.handlers())

	// Who's there.
	waitUntil(t, sfuWait, "ann to see bob and the whiteboard arrive", func() bool { return ann.has("arrived bob") && ann.has("arrived wb") })
	ann.mu.Lock()
	got := ann.arrived["wb"]
	ann.mu.Unlock()
	if !got.Agent || got.Name != "WB" {
		t.Errorf("the whiteboard arrived as %+v", got)
	}
	ps := w.participants()
	sort.Slice(ps, func(i, j int) bool { return ps[i].Identity < ps[j].Identity })
	if len(ps) != 2 || ps[0] != (rtc.Participant{Identity: "ann", Name: "ANN"}) || ps[1].Identity != "bob" || ps[1].Agent {
		t.Errorf("the whiteboard found %+v", ps)
	}
	if self := w.self(); self.Identity != "wb" || !self.Agent || w.roomName() != "r" {
		t.Errorf("the whiteboard joined %q as %+v", w.roomName(), self)
	}
	if b.estimator() == nil {
		t.Error("no estimator on bob's publish connection after the join")
	}

	// Bob talks: ann is sent his voice, Opus with the audio level, and the
	// SFU's sender reports reach the room's hook.
	out, err := b.publishAudio()
	if err != nil {
		t.Fatal(err)
	}
	v := speakOn(out, 20)
	defer v.halt()
	waitUntil(t, sfuWait, "ann to be sent bob's voice", func() bool { return ann.track("bob", webrtc.RTPCodecTypeAudio) != nil })
	h := ann.track("bob", webrtc.RTPCodecTypeAudio)
	if h.rt.sid != out.sid() || h.rt.source != sourceMic || !strings.EqualFold(h.rt.mime, webrtc.MimeTypeOpus) || h.rt.levelExt == 0 || h.rt.video != nil {
		t.Errorf("bob's voice came as %+v", h.rt)
	}
	waitUntil(t, sfuWait, "a sender report on bob's voice", func() bool { return h.srs.Load() > 0 })
	if n, l := h.packets.Load(), h.levels.Load(); n == 0 || l != n {
		t.Errorf("ann read %d packets of bob's, %d with the level", n, l)
	}
	waitUntil(t, sfuWait, "bob among the speakers", func() bool { return ann.lastSpeakers()["bob"] > 0 })

	// Bob mutes but keeps writing loud frames: the SFU hears the mute (he
	// goes off the speakers) and still forwards what comes.
	out.setMuted(true)
	waitUntil(t, sfuWait, "bob off the speakers once muted", func() bool {
		sp := ann.lastSpeakers()
		return sp != nil && sp["bob"] == 0
	})
	before := h.packets.Load()
	time.Sleep(200 * time.Millisecond)
	if h.packets.Load() == before {
		t.Error("nothing forwarded while muted and writing")
	}
	out.setMuted(false)
	waitUntil(t, sfuWait, "bob back among the speakers", func() bool { return ann.lastSpeakers()["bob"] > 0 })

	// Bob adds a camera, three layers: every layer reaches the SFU (its
	// receiver reports name each layer's SSRC), ann is sent the track, and
	// what she asks of it goes without error.
	layers := []rtc.Layer{{W: 1280, H: 720, Bitrate: 1_700_000}, {W: 640, H: 360, Bitrate: 450_000}, {W: 320, H: 180, Bitrate: 160_000}}
	var rrMu sync.Mutex
	named := map[uint32]bool{}
	vo, err := b.publishVideo(rtc.Camera, layers, func(layer int, p rtcp.Packet) {
		if rr, ok := p.(*rtcp.ReceiverReport); ok {
			rrMu.Lock()
			for _, r := range rr.Reports {
				named[r.SSRC] = true
			}
			rrMu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		au := append([]byte{0, 0, 0, 1, 0x65}, make([]byte, 3000)...) // a stand-in IDR unit, three packets
		for ts := uint32(0); ; ts += 3000 {
			select {
			case <-stop:
				return
			case <-time.After(33 * time.Millisecond):
			}
			for l := range layers {
				vo.write(l, au, ts)
			}
		}
	}()
	sv := vo.(*sfuVideo)
	waitUntil(t, sfuWait, "the SFU's receiver reports on every layer", func() bool {
		rrMu.Lock()
		defer rrMu.Unlock()
		for _, tr := range sv.tracks {
			if tr.SSRC() == 0 || !named[uint32(tr.SSRC())] {
				return false
			}
		}
		return true
	})
	waitUntil(t, sfuWait, "ann to be sent bob's camera", func() bool { return ann.track("bob", webrtc.RTPCodecTypeVideo) != nil })
	hv := ann.track("bob", webrtc.RTPCodecTypeVideo)
	if hv.rt.sid != vo.sid() || hv.rt.source != sourceCamera || !strings.EqualFold(hv.rt.mime, webrtc.MimeTypeH264) || hv.rt.video == nil {
		t.Errorf("bob's camera came as %+v", hv.rt)
	} else {
		hv.rt.video.setSize(640, 360)
		hv.rt.video.setEnabled(false)
		hv.rt.video.setEnabled(true)
		hv.rt.video.pli()
		if err := a.s.PLI(hv.rt.track.SSRC()); err != nil {
			t.Errorf("a PLI: %v", err)
		}
	}
	close(stop)
	<-done
	if err := vo.unpublish(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, sfuWait, "ann to lose bob's camera", func() bool { return ann.has("unsubscribed bob video") && hv.ended.Load() })

	// The whiteboard leaves; then bob, his voice going first.
	w.leave()
	waitUntil(t, sfuWait, "ann to hear the whiteboard leave", func() bool { return ann.has("left wb") })
	v.halt()
	b.leave()
	waitUntil(t, sfuWait, "ann to hear bob leave", func() bool { return ann.has("left bob") })
	ev := ann.eventLog()
	if i, j := slices.Index(ev, "unsubscribed bob audio"), slices.Index(ev, "left bob"); i < 0 || i > j {
		t.Errorf("ann's events: %q", ev)
	}
	if wb.has("disconnected") || bob.has("disconnected") {
		t.Errorf("leaving told the room disconnected: %q, %q", wb.eventLog(), bob.eventLog())
	}

	// Someone else joins as ann: the server ends her first session, and
	// the room hears why.
	dialTest(t, ts, "r", "ann", false, newRecorder().handlers())
	waitUntil(t, sfuWait, "ann's first session to end", func() bool { return ann.has("disconnected") })
	ann.mu.Lock()
	reason := ann.reason
	ann.mu.Unlock()
	if !strings.Contains(reason, "the server ended the session") {
		t.Errorf("disconnected: %q", reason)
	}
	for _, e := range ann.eventLog() {
		if e == "reconnecting" || e == "reconnected" {
			t.Errorf("ann's events: %q", ann.eventLog())
		}
	}
}

// TestBuiltinVoice is the room over the builtin transport, as the app uses
// it: bob's mic (a fileio click train, Opus) through the SFU into ann's
// jitter buffer and mix, at 40 ms from the first second with nothing lost;
// his talking shown and the SFU's speakers heard; the whiteboard (an
// agent) arriving; a mute; and bob leaving.
func TestBuiltinVoice(t *testing.T) {
	ts := newTestSFU(t)
	ctx, cancel := context.WithTimeout(context.Background(), sfuWait)
	defer cancel()
	c := &Connector{StatsEvery: -1}

	var mu sync.Mutex
	var annEvents []string
	arrived := map[string]rtc.Participant{}
	loudest := map[string]float32{}
	note := func(e string) {
		mu.Lock()
		annEvents = append(annEvents, e)
		mu.Unlock()
	}
	hasEvent := func(e string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(annEvents, e)
	}
	ann, err := c.Connect(ctx, ts.join(t, "v", "ann", false), rtc.Events{
		Joined: func(self rtc.Participant, others []rtc.Participant) { note("joined " + self.Identity) },
		Arrived: func(p rtc.Participant) {
			mu.Lock()
			arrived[p.Identity] = p
			mu.Unlock()
			note("arrived " + p.Identity)
		},
		Left: func(id string) { note("left " + id) },
		Levels: func(ls []rtc.Level) {
			mu.Lock()
			for _, l := range ls {
				if v, ok := loudest[l.Identity]; !ok || l.DBFS > v {
					loudest[l.Identity] = l.DBFS
				}
			}
			mu.Unlock()
		},
		Disconnected: func(err error) { note("disconnected " + err.Error()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ann.Leave()
	spk := &fileio.Speaker{}
	if err := ann.Play(spk); err != nil {
		t.Fatal(err)
	}
	if !hasEvent("joined ann") {
		t.Errorf("ann's events: %q", annEvents)
	}

	bob, err := c.Connect(ctx, ts.join(t, "v", "bob", false), rtc.Events{})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Leave()
	const rate = rtc.SampleRate
	clicks := pcm.ClickTrain(10, rate/2, rate*3/10, rate) // 10 clicks 300 ms apart from 0.5 s in: 3.5 s
	mic := &fileio.Mic{Samples: clicks}
	if err := bob.PublishMic(mic); err != nil {
		t.Fatal(err)
	}

	// ann's jitter buffer for bob, every 50 ms from his first packet until
	// the clicks are over.
	stats := func() (target time.Duration, received, lost int, ok bool) {
		s, ok := TrackStats(ann)["bob"]
		return s.Target, s.Received, s.Lost, ok
	}
	waitUntil(t, sfuWait, "ann to hear bob", func() bool { _, n, _, ok := stats(); return ok && n > 0 })
	first := time.Now()
	var targets []time.Duration
	for !mic.Done() {
		tg, _, _, _ := stats()
		targets = append(targets, tg)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // the tail
	target, received, lost, _ := stats()
	t.Logf("bob heard %v after his first packet: %d packets, %d lost, jitter delay %v; sampled %d times: %v",
		time.Since(first).Round(time.Millisecond), received, lost, target, len(targets), summarize(targets))
	for i, tg := range targets {
		if tg != 40*time.Millisecond {
			t.Errorf("jitter delay %v at sample %d (%v after the first packet), want 40ms throughout", tg, i, time.Duration(i)*50*time.Millisecond)
			break
		}
	}
	if lost != 0 || received < 20 {
		t.Errorf("received %d packets, lost %d", received, lost)
	}

	// The clicks came through the codec and the mix: each one sent after
	// the mic was negotiated is heard once, with a latency like a call's.
	sent := pcm.Onsets(clicks, 0.02, rate/10, rate)
	heard := pcm.Onsets(spk.Samples(), 0.02, rate/10, rate)
	lag := func(s, h int) time.Duration {
		return spk.Start().Add(time.Duration(h) * time.Second / rate).Sub(mic.Start().Add(time.Duration(s) * time.Second / rate))
	}
	var lats []time.Duration
	for _, h := range heard {
		var best time.Duration = -1
		for _, s := range sent {
			if d := lag(s, h); d >= 0 && (best < 0 || d < best) {
				best = d
			}
		}
		if best >= 0 {
			lats = append(lats, best)
		}
	}
	t.Logf("clicks: %d sent, %d heard; latencies %v", len(sent), len(heard), summarize(lats))
	if len(heard) < len(sent)-1 || len(heard) > len(sent) {
		t.Errorf("heard %d clicks of %d", len(heard), len(sent))
	}
	if len(lats) > 0 && median(lats) > 150*time.Millisecond {
		t.Errorf("click latency median %v", median(lats))
	}

	// His talking was shown, and the SFU's speakers named him.
	mu.Lock()
	lv := loudest["bob"]
	mu.Unlock()
	if lv < -30 {
		t.Errorf("bob's loudest level in ann's mix %.1f dBFS", lv)
	}
	r := ann.(*room)
	r.mu.Lock()
	sfuLevel := r.sfuLevel["bob"]
	r.mu.Unlock()
	if sfuLevel <= 0 {
		t.Error("the SFU never named bob a speaker")
	}

	// The whiteboard joins: an agent to ann.
	wb, err := c.Connect(ctx, ts.join(t, "v", "wb", true), rtc.Events{})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, sfuWait, "ann to see the whiteboard arrive", func() bool { return hasEvent("arrived wb") })
	mu.Lock()
	if p := arrived["wb"]; !p.Agent || p.Name != "WB" {
		t.Errorf("the whiteboard arrived as %+v", p)
	}
	mu.Unlock()
	var agents []string
	for _, p := range ann.Participants() {
		if p.Agent {
			agents = append(agents, p.Identity)
		}
	}
	if !slices.Equal(agents, []string{"wb"}) {
		t.Errorf("agents among ann's participants: %q", agents)
	}

	// Bob mutes: one silent frame, then nothing.
	bob.Mute(true)
	time.Sleep(300 * time.Millisecond)
	_, atMute, _, _ := stats()
	time.Sleep(700 * time.Millisecond)
	if _, n, _, _ := stats(); n != atMute {
		t.Errorf("%d packets from bob while muted", n-atMute)
	}

	// The whiteboard and bob leave: ann hears both go, and stops hearing bob.
	wb.Leave()
	bob.Leave()
	waitUntil(t, sfuWait, "ann to hear them leave", func() bool { return hasEvent("left wb") && hasEvent("left bob") })
	waitUntil(t, sfuWait, "bob's track gone from ann", func() bool { _, _, _, ok := stats(); return !ok })
	mu.Lock()
	defer mu.Unlock()
	for _, e := range annEvents {
		if strings.HasPrefix(e, "disconnected") {
			t.Errorf("ann's events: %q", annEvents)
		}
	}
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

// summarize is the distinct values of ds, in order, with how many times
// each came in a row.
func summarize(ds []time.Duration) string {
	var b strings.Builder
	for i := 0; i < len(ds); {
		j := i
		for j < len(ds) && ds[j] == ds[i] {
			j++
		}
		fmt.Fprintf(&b, "%v×%d ", ds[i].Round(time.Millisecond), j-i)
		i = j
	}
	return strings.TrimSpace(b.String())
}
