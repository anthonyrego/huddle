package sfu

// The rest of the SFU's step-1 gate, end to end on loopback: a late
// joiner, sender reports, speakers, joins turned away, one identity twice,
// someone leaving, a video publish, and congestion control's estimator.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

func TestLateJoiner(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	sp, _ := publishVoice(t, a, 25)
	waitFor(t, wait, "a to be speaking", func() bool { return sp.sent.Load() > 25 })

	d := ts.join(t, "r", "d", sfuclient.Options{})
	waitFor(t, wait, "d to hear a", func() bool { return len(d.framesFrom("a")) > 0 })
	time.Sleep(time.Second)
	sent := allHeard(t, []*testClient{d}, []*speaker{sp})

	d.mu.Lock()
	h := d.fromLocked("a", signal.Audio)
	connected, first, added := d.subConnected, h.firstAt, h.addedAt
	frames, tss, bad, packets := slices.Clone(h.frames), slices.Clone(h.tss), slices.Clone(h.bad), h.packets
	d.mu.Unlock()
	// The connection's event and the track's come from goroutines of their
	// own, so the first packet may be read before the event is delivered:
	// less than no time.
	delay := first.Sub(connected)
	t.Logf("d's receive connection up to a's first packet: %v (track added %v after it); %d packets, frames %d–%d of %d sent",
		delay.Round(100*time.Microsecond), added.Sub(connected).Round(100*time.Microsecond), packets, frames[0], frames[len(frames)-1], sent["a"])
	if connected.IsZero() || delay > 100*time.Millisecond {
		t.Errorf("first packet %v after the receive connection connected, want within 100 ms", delay)
	}
	if len(bad) > 0 || packets != len(frames) {
		t.Errorf("%d packets, %d of them frames a wrote; not: %q", packets, len(frames), bad)
	}
	checkRun(t, "d hearing a", frames, frames[0], sent["a"]-1)
	for i, n := range frames {
		if tss[i]-tss[0] != sp.tsOf(n)-sp.tsOf(frames[0]) {
			t.Errorf("frame %d at RTP %d, not where a wrote it", n, tss[i])
			break
		}
	}
}

func TestSenderReports(t *testing.T) {
	ts := newTestSFU(t, nil)
	// The publisher's own reports, as its connection sends them. Its frames
	// are stamped from the clock, so its reports map RTP time to when
	// frames were written however late its timer wakes.
	var mine srRecorder
	a := ts.join(t, "r", "a", sfuclient.Options{Publish: []interceptor.Factory{&mine}})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	sp, _ := publishSpeaker(t, a, 25, true)
	waitFor(t, wait, "b to hear a", func() bool { return len(b.framesFrom("a")) > 0 })
	waitFor(t, 3*wait, "four sender reports on b's track", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.fromLocked("a", signal.Audio).srs) >= 4
	})
	sp.halt()

	b.mu.Lock()
	h := b.fromLocked("a", signal.Audio)
	srs, frames, tss, firstAt := slices.Clone(h.srs), slices.Clone(h.frames), slices.Clone(h.tss), h.firstAt
	b.mu.Unlock()
	pubSRs := mine.reports()
	if len(pubSRs) < 2 {
		t.Fatalf("the publisher sent %d sender reports", len(pubSRs))
	}
	downTS := map[int]uint32{} // the down track's RTP timestamp of each frame
	for i, n := range frames {
		downTS[n] = tss[i]
	}
	n0 := frames[0]
	t.Logf("first report %v after the first packet (the publisher's first came %v after it)",
		srs[0].at.Sub(firstAt).Round(time.Millisecond), pubSRs[0].at.Sub(firstAt).Round(time.Millisecond))
	if first := srs[0].at.Sub(pubSRs[0].at); first > 100*time.Millisecond {
		t.Errorf("the SFU's first report came %v after the publisher's first", first)
	}
	for i := 1; i < len(srs); i++ {
		if gap := srs[i].at.Sub(srs[i-1].at); gap < 800*time.Millisecond || gap > 1500*time.Millisecond {
			t.Errorf("reports %d and %d %v apart, want about a second", i-1, i, gap)
		}
	}

	// Two RTP times a second apart, each mapped through a different report
	// of the subscriber's, are a second apart on the SFU's clock.
	var worst time.Duration
	for i := 1; i < len(srs); i++ {
		r1 := downTS[n0+10*i]
		r2 := r1 + 48000
		t1, t2 := srTime(srs[i-1], r1), srTime(srs[i], r2)
		err := t2.Sub(t1) - time.Second
		worst = max(worst, err.Abs())
		t.Logf("RTP %d and %d, through reports %d and %d (%v apart): %v apart", r1, r2, i-1, i,
			srs[i].at.Sub(srs[i-1].at).Round(time.Millisecond), t2.Sub(t1))
	}
	t.Logf("a second of the publisher's, through the SFU's reports: off by %v at most", worst)
	if worst > 5*time.Millisecond {
		t.Errorf("a second of RTP mapped to a second off by %v, want within 5 ms", worst)
	}

	// And they agree with the publisher's own (one clock here): each maps
	// frame n0 to the time the publisher's report before it does, later by
	// how long that report took to reach the SFU.
	var offs []time.Duration
	for _, sr := range srs {
		var p srSeen
		for _, q := range pubSRs {
			if q.at.Before(sr.at) {
				p = q
			}
		}
		offs = append(offs, srTime(sr, downTS[n0]).Sub(srTime(p, sp.tsOf(n0))))
	}
	lo, hi := slices.Min(offs), slices.Max(offs)
	t.Logf("the SFU's reports against the publisher's: %v to %v later", lo.Round(100*time.Microsecond), hi.Round(100*time.Microsecond))
	if lo < -5*time.Millisecond || hi > 25*time.Millisecond || hi-lo > 5*time.Millisecond {
		t.Errorf("the SFU's reports map RTP %v to %v after the publisher's, want 0–25 ms and steady within 5 ms", lo, hi)
	}
}

// srTime is when, by report r, RTP time ts was.
func srTime(r srSeen, ts uint32) time.Time {
	sec, frac := r.ntp>>32, r.ntp&0xffffffff
	at := time.Unix(int64(sec)-ntpEpoch, int64(frac*1e9>>32))
	return at.Add(time.Duration(float64(int32(ts-r.rtp)) / 48000 * float64(time.Second)))
}

// srRecorder is an interceptor that keeps the sender reports a connection
// sends.
type srRecorder struct {
	interceptor.NoOp
	mu  sync.Mutex
	srs []srSeen
}

func (r *srRecorder) NewInterceptor(string) (interceptor.Interceptor, error) { return r, nil }

func (r *srRecorder) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, a interceptor.Attributes) (int, error) {
		now := time.Now()
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok && sr.PacketCount > 0 {
				r.mu.Lock()
				r.srs = append(r.srs, srSeen{ntp: sr.NTPTime, rtp: sr.RTPTime, at: now})
				r.mu.Unlock()
			}
		}
		return w.Write(pkts, a)
	})
}

func (r *srRecorder) reports() []srSeen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.srs)
}

func TestSpeakers(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	loud, _ := publishVoice(t, a, 10)    // −10 dBov
	quiet, _ := publishVoice(t, b, 30)   // −30 dBov
	silent, _ := publishVoice(t, c, 127) // nothing

	last := func(x *testClient) []signal.Speaker {
		x.mu.Lock()
		defer x.mu.Unlock()
		if len(x.speakers) == 0 {
			return nil
		}
		return x.speakers[len(x.speakers)-1]
	}
	start := time.Now()
	for _, x := range []*testClient{a, b, c} {
		waitFor(t, wait, x.id+" to hear a, then b, talking", func() bool {
			sp := last(x)
			return len(sp) == 2 && sp[0].Identity == "a" && sp[1].Identity == "b"
		})
	}
	sp := last(c)
	t.Logf("speakers %+v, %v after they began", sp, time.Since(start).Round(time.Millisecond))
	if sp[0].Level <= sp[1].Level || sp[0].Level < 0.2 {
		t.Errorf("levels %+v: want a loud, and louder than b", sp)
	}
	for _, s := range []*speaker{loud, quiet, silent} {
		s.level.Store(127)
	}
	start = time.Now()
	for _, x := range []*testClient{a, b, c} {
		waitFor(t, wait, x.id+" to hear no one talking", func() bool {
			sp := last(x)
			return sp != nil && len(sp) == 0
		})
	}
	t.Logf("silence to an empty list: %v", time.Since(start).Round(time.Millisecond))
	c.mu.Lock()
	for i, sp := range c.speakers {
		for _, s := range sp {
			if s.Identity == "c" {
				t.Errorf("speakers message %d lists the silent c: %+v", i, sp)
			}
		}
	}
	c.mu.Unlock()
}

func TestTurnedAway(t *testing.T) {
	ts := newTestSFU(t, nil)
	late := newTestSFU(t, func(s *Server) { s.now = func() time.Time { return time.Now().Add(time.Hour) } })
	other, err := rtctoken.Minter{Secret: []byte("another secret")}.Mint(rtctoken.Grant{Room: "r", Identity: "a"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, url string
		join      signal.Join
		want      string
	}{
		{"bad token", ts.url, signal.Join{Protocol: signal.Protocol, Token: "not.a token"}, "not a valid token"},
		{"another secret", ts.url, signal.Join{Protocol: signal.Protocol, Token: other}, "not a valid token"},
		{"expired", late.url, signal.Join{Protocol: signal.Protocol, Token: late.token(t, "r", "a")}, "expired"},
		{"wrong protocol", ts.url, signal.Join{Protocol: signal.Protocol + 1, Token: ts.token(t, "r", "a")}, "protocol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			frame, _ := signal.Marshal(signal.KindJoin, tc.join)
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				t.Fatal(err)
			}
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("no answer: %v", err)
			}
			e, err := decode(data)
			if err != nil {
				t.Fatal(err)
			}
			var m signal.Error
			e.Decode(&m)
			if e.Kind != signal.KindError || !strings.Contains(m.Message, tc.want) {
				t.Errorf("answered %s %q, want an error saying %q", e.Kind, m.Message, tc.want)
			}
			_, _, err = conn.Read(ctx)
			if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
				t.Errorf("then %v, want closed (policy violation)", err)
			}
		})
	}
	// A session's Join says why.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, err = sfuclient.Join(ctx, late.url, late.token(t, "r", "a"), sfuclient.Options{Loopback: true}, sfuclient.Events{})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("Join with an expired token: %v", err)
	}
	if ts.s.room("r") != nil || late.s.room("r") != nil {
		t.Errorf("a room made for no one")
	}
}

func TestSameIdentity(t *testing.T) {
	ts := newTestSFU(t, nil)
	a1 := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	sp, _ := publishVoice(t, a1, 25)
	waitFor(t, wait, "b to hear a", func() bool { return len(b.framesFrom("a")) > 0 })

	a2 := ts.join(t, "r", "a", sfuclient.Options{})
	waitFor(t, wait, "the first a to be told bye", func() bool {
		a1.mu.Lock()
		defer a1.mu.Unlock()
		return a1.disconnected != nil
	})
	a1.mu.Lock()
	err := a1.disconnected
	a1.mu.Unlock()
	if !errors.Is(err, sfuclient.ErrBye) || !strings.Contains(err.Error(), "another session joined as a") {
		t.Errorf("the first a disconnected with %v, want bye", err)
	}
	sp.halt()
	waitFor(t, wait, "b to see a leave and come back", func() bool { return b.has("arrived a") })
	before(t, b, "removed a audio", "left a")
	before(t, b, "left a", "arrived a")
	waitFor(t, wait, "the second a's joined", func() bool { return len(a2.eventLog()) > 0 })
	if got := a2.eventLog()[0]; got != "joined b" {
		t.Errorf("the second a's first event %q, want joined b", got)
	}
	// The second a publishes and is heard from its first frame.
	sp2, _ := publishVoice(t, a2, 25)
	waitFor(t, wait, "b to hear the second a", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		h := b.fromLocked("a", signal.Audio)
		return h != nil && !h.ended && len(h.frames) > 10
	})
	sp2.halt()
}

// TestSendOnlyAndMute checks a session that subscribes to nothing (a bot):
// it hears who's there and what they publish, is offered nothing, and is
// heard. Its mute reaches the others, and takes it off the speakers.
func TestSendOnlyAndMute(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	spA, _ := publishVoice(t, a, 20)
	bot := ts.join(t, "r", "bot", sfuclient.Options{SendOnly: true})
	spBot, sid := publishVoice(t, bot, 10)
	waitFor(t, wait, "a to hear the bot", func() bool { return len(a.framesFrom("bot")) > 10 })
	waitFor(t, wait, "the bot to hear of a and its track", func() bool { return bot.has("joined a") })
	if got := bot.s.Participants(); len(got) != 1 || got[0].Identity != "a" || len(got[0].Tracks) != 1 {
		t.Errorf("the bot's participants: %+v, want a and its voice", got)
	}
	time.Sleep(300 * time.Millisecond)
	if p := ts.s.room("r").participant("bot"); p.down.mlines() != 0 || bot.from("a", signal.Audio) != nil {
		t.Errorf("a session that subscribes to nothing was offered a's voice")
	}
	waitFor(t, wait, "a to hear the bot loudest", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		n := len(a.speakers)
		return n > 0 && len(a.speakers[n-1]) == 2 && a.speakers[n-1][0].Identity == "bot"
	})

	// Muted, though it goes on sending (the app stops; the SFU doesn't
	// count on it).
	if err := bot.s.Mute(sid, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, wait, "a to hear the bot muted", func() bool { return a.has("info bot audio muted=true") })
	waitFor(t, wait, "the bot off the speakers", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		n := len(a.speakers)
		return n > 0 && len(a.speakers[n-1]) == 1 && a.speakers[n-1][0].Identity == "a"
	})
	spBot.halt()
	spA.halt()
}

// TestServerCloses checks that a closing server tells everyone bye, and
// forgets its rooms.
func TestServerCloses(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "s", "b", sfuclient.Options{})
	sp, _ := publishVoice(t, a, 25)
	waitFor(t, wait, "a to send", func() bool { return sp.sent.Load() > 5 })
	start := time.Now()
	if err := ts.s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("closed in %v", time.Since(start).Round(time.Millisecond))
	for _, x := range []*testClient{a, b} {
		x.mu.Lock()
		err := x.disconnected
		x.mu.Unlock()
		if err == nil {
			waitFor(t, wait, x.id+" to be told", func() bool { return x.has("disconnected") })
			x.mu.Lock()
			err = x.disconnected
			x.mu.Unlock()
		}
		if !errors.Is(err, sfuclient.ErrBye) || !strings.Contains(err.Error(), "closing") {
			t.Errorf("%s: disconnected with %v, want bye", x.id, err)
		}
	}
	if ts.s.room("r") != nil || ts.s.room("s") != nil {
		t.Error("rooms left after closing")
	}
}

func TestLeaving(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	spA, _ := publishVoice(t, a, 25)
	spB, _ := publishVoice(t, b, 25)
	spC, sidC := publishVoice(t, c, 25)
	hearsAll(t, []*testClient{a, b, c}, []*speaker{spA, spB, spC})
	readyAt := map[string]int{"a": int(spA.sent.Load()), "b": int(spB.sent.Load())}

	spC.halt()
	c.s.Close()
	for _, x := range []*testClient{a, b} {
		waitFor(t, wait, x.id+" to hear c leave and its track come off", func() bool {
			x.mu.Lock()
			defer x.mu.Unlock()
			h := x.fromLocked("c", signal.Audio)
			return slices.Contains(x.events, "left c") && h != nil && h.ended
		})
		before(t, x, "removed c audio", "left c")
		// The SFU's side: c's track is off x's down connection.
		if ts.s.room("r").participant(x.id).down.sending(sidC) {
			t.Errorf("%s: c's track still on the down connection", x.id)
		}
	}
	mlines := ts.s.room("r").participant("a").down.mlines()

	// Someone new publishes, on the m-line c's track left.
	d := ts.join(t, "r", "d", sfuclient.Options{})
	spD, _ := publishVoice(t, d, 25)
	waitFor(t, wait, "a and b to hear d", func() bool { return len(a.framesFrom("d")) > 5 && len(b.framesFrom("d")) > 5 })
	if got := ts.s.room("r").participant("a").down.mlines(); got != mlines {
		t.Errorf("a's down connection has %d m-lines, was %d: c's wasn't used again", got, mlines)
	}
	sent := allHeard(t, []*testClient{a, b}, []*speaker{spA, spB, spD})
	checkRun(t, "a hearing b through it", a.framesFrom("b"), readyAt["b"], sent["b"]-1)
	checkRun(t, "b hearing a through it", b.framesFrom("a"), readyAt["a"], sent["a"]-1)
}

func TestVideoPublish(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	sp, _ := publishVoice(t, a, 25)
	hearsAll(t, []*testClient{b, c}, []*speaker{sp})
	readyAt := int(sp.sent.Load())

	// a adds a camera, three layers on one transceiver, while it talks.
	var layers []*sfuclient.Track
	for _, rid := range []string{"q", "h", "f"} {
		tr, err := sfuclient.NewTrack(codecs.H264, "cam", "a", rid, nil)
		if err != nil {
			t.Fatal(err)
		}
		layers = append(layers, tr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	sid, err := a.s.Publish(ctx, signal.Publish{CID: "cam", Kind: signal.Video, Source: signal.Camera,
		Layers: []signal.Layer{{RID: "f", W: 1280, H: 720}, {RID: "h", W: 640, H: 360}, {RID: "q", W: 320, H: 180}}}, layers...)
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for ts := uint32(0); ; ts += 3000 {
			select {
			case <-stop:
				return
			case <-time.After(33 * time.Millisecond):
			}
			for _, l := range layers {
				l.WriteFrame(frame, ts, nil)
			}
		}
	}()
	for _, x := range []*testClient{b, c} {
		waitFor(t, wait, x.id+" to be offered a's camera", func() bool { return x.has("added a video") })
	}
	pub := ts.s.room("r").participant("a").publication(sid)
	waitFor(t, wait, "the SFU to receive the three layers", func() bool {
		pub.lmu.Lock()
		defer pub.lmu.Unlock()
		for _, l := range pub.layers {
			if l.ssrc == 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(time.Second)
	close(stop)
	<-done
	sent := allHeard(t, []*testClient{b, c}, []*speaker{sp})
	for _, x := range []*testClient{b, c} {
		checkRun(t, x.id+" hearing a through the camera's negotiation", x.framesFrom("a"), readyAt, sent["a"]-1)
		x.mu.Lock()
		// The stand-in frames carry no SPS, so none starts a keyframe
		// the forwarder could begin at (video_test.go sends real ones).
		if h := x.fromLocked("a", signal.Video); h == nil || h.packets != 0 {
			t.Errorf("%s: sent frames that aren't a keyframe's", x.id)
		}
		x.mu.Unlock()
	}
}

// TestPublishEstimator checks that congestion control given in
// Options.Publish is the publish connection's, handed over during Join.
func TestPublishEstimator(t *testing.T) {
	ts := newTestSFU(t, nil)
	f, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(1_000_000), gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ests []cc.BandwidthEstimator
	f.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) {
		mu.Lock()
		ests = append(ests, e)
		mu.Unlock()
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(ests)
	}
	a := ts.join(t, "r", "a", sfuclient.Options{Publish: []interceptor.Factory{f}})
	if n := count(); n != 1 {
		t.Fatalf("%d estimators by the time Join returned, want the publish connection's", n)
	}
	start := time.Now()
	sp, _ := publishVoice(t, a, 25)
	// The SFU's TWCC feedback moves the estimate off where it began.
	waitFor(t, wait, "the estimate to move", func() bool { return ests[0].GetTargetBitrate() != 1_000_000 })
	t.Logf("estimate %d bps %v after publishing (%d frames sent)", ests[0].GetTargetBitrate(), time.Since(start).Round(time.Millisecond), sp.sent.Load())
	if n := count(); n != 1 {
		t.Errorf("%d estimators after publishing", n)
	}
}

// A client that never answers the server's offer is sent bye after
// answerWait: nothing more could ever be sent to it.
func TestUnansweredOffer(t *testing.T) {
	ts := newTestSFU(t, func(s *Server) { s.answerWait = 300 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, ts.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	frame, _ := signal.Marshal(signal.KindJoin, signal.Join{Protocol: signal.Protocol, Token: ts.token(t, "r", "mute")})
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	a := ts.join(t, "r", "a", sfuclient.Options{})
	publishVoice(t, a, 30)
	var offered time.Time
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("closed before bye: %v", err)
		}
		e, err := decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == signal.KindOffer && offered.IsZero() {
			offered = time.Now()
		}
		if e.Kind == signal.KindBye {
			var b signal.Bye
			e.Decode(&b)
			if offered.IsZero() || !strings.Contains(b.Reason, "no answer") {
				t.Fatalf("bye %q (offered: %v)", b.Reason, !offered.IsZero())
			}
			t.Logf("bye %v after the offer: %s", time.Since(offered).Round(time.Millisecond), b.Reason)
			break
		}
	}
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break // the server's close, answered
		}
	}
	waitFor(t, wait, "the silent client to leave", func() bool { return a.has("left mute") })
}
