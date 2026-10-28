package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/fixturemeet"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/wire"
)

// builtinServer is a builtin huddled's HTTP API on loopback: the dev join
// and the SFU's /rtc. It returns the server's URL and its media.
func builtinServer(t *testing.T) (string, *media) {
	t.Helper()
	t.Setenv("HUDDLE_MEDIA", "builtin")
	t.Setenv("HUDDLE_RTC_SECRET", "")
	t.Setenv("HUDDLE_NODE_IP", "127.0.0.1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := newMedia(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	api := &httpapi.Server{Rooms: rooms{}, Dev: true}
	m.api(api)
	srv := &http.Server{Handler: api.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); m.close() })
	return "http://" + ln.Addr().String(), m
}

// tagged is frame n of a test voice: an Opus TOC byte (20 ms of CELT), the
// voice's tag and n.
func tagged(tag byte, n uint32) []byte { return binary.BigEndian.AppendUint32([]byte{0xf8, tag}, n) }

func untag(b []byte) (byte, uint32, bool) {
	if len(b) != 6 || b[0] != 0xf8 {
		return 0, 0, false
	}
	return b[1], binary.BigEndian.Uint32(b[2:]), true
}

// talk writes a tagged frame every 20 ms to w until ctx ends, keeping when
// each was written.
type talk struct {
	mu sync.Mutex
	at []time.Time
}

func (tk *talk) run(ctx context.Context, w fixturemeet.FrameWriter, tag byte) {
	lvl := uint8(30)
	start := time.Now()
	for n := uint32(0); ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(time.Duration(n) * 20 * time.Millisecond))):
		}
		tk.mu.Lock()
		tk.at = append(tk.at, time.Now())
		tk.mu.Unlock()
		w.WriteFrame(tagged(tag, n), n*960, &lvl)
	}
}

func (tk *talk) when(n uint32) (time.Time, bool) {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if int(n) >= len(tk.at) {
		return time.Time{}, false
	}
	return tk.at[n], true
}

// got is one voice as a listener heard it.
type got struct {
	who, name string
	tags      map[byte]int // frames by tag
	frames    []uint32     // the frames, in order
	at        []time.Time
}

// listen keeps each voice a listener hears, by track SID.
type listen struct {
	mu     sync.Mutex
	voices map[string]*got
}

func (l *listen) read(sid, who, name string, read func() ([]byte, error)) {
	g := &got{who: who, name: name, tags: map[byte]int{}}
	l.mu.Lock()
	l.voices[sid] = g
	l.mu.Unlock()
	for {
		b, err := read()
		if err != nil {
			return
		}
		tag, n, ok := untag(b)
		if !ok {
			continue
		}
		l.mu.Lock()
		g.tags[tag]++
		g.frames = append(g.frames, n)
		g.at = append(g.at, time.Now())
		l.mu.Unlock()
	}
}

func (l *listen) from(who string) []*got {
	l.mu.Lock()
	defer l.mu.Unlock()
	var gs []*got
	for _, g := range l.voices {
		if g.who == who {
			gs = append(gs, g)
		}
	}
	return gs
}

func (l *listen) frames(who string, tag byte) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, g := range l.voices {
		if g.who == who {
			n += g.tags[tag]
		}
	}
	return n
}

// TestBotsOnBuiltin puts the bots in a builtin room through the dev join,
// beside the whiteboard, a person (Ann) and a listener (Bea): a fixture
// bot's microphone is heard by the whiteboard and Bea; the echo bot
// repeats Ann once, 1.5 s later, and never the fixture bot or the
// whiteboard.
func TestBotsOnBuiltin(t *testing.T) {
	server, m := builtinServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const room = "bots"
	join := func(name string, bot bool) wire.Joined {
		j, err := devjoin.Join(ctx, server, wire.DevJoin{Room: room, Name: name, Bot: bot})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	// The whiteboard, as serve runs it, hears and speaks.
	wb := &listen{voices: map[string]*got{}}
	mr, err := m.agent.Join(ctx, room, agent.Hearing{
		Arrived: func(string, string) {},
		Left:    func(string) {},
		Voice: func(identity, name string, v agent.VoiceTrack) {
			go wb.read(identity, identity, name, func() ([]byte, error) {
				p, _, err := v.ReadRTP()
				if err != nil {
					return nil, err
				}
				return p.Payload, nil
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Leave()
	var wbTalk talk
	go wbTalk.run(ctx, writerFunc(func(f []byte, ts uint32, _ *uint8) error { return mr.Publish(f, ts) }), 'w')

	// The echo bot.
	je := join("Echo", true)
	echoCtx, stopEcho := context.WithCancel(ctx)
	echoDone := make(chan error, 1)
	go func() { echoDone <- runEcho(echoCtx, devjoin.RTC(je), 1500*time.Millisecond, sfuclient.Options{}) }()

	// Bea listens.
	bea := &listen{voices: map[string]*got{}}
	jb := join("Bea", false)
	bs, err := sfuclient.Join(ctx, jb.MediaURL, jb.Token, sfuclient.Options{}, sfuclient.Events{
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			go bea.read(rt.SID, rt.Participant, rt.Name, func() ([]byte, error) {
				p, _, err := rt.Track.ReadRTP()
				if err != nil {
					return nil, err
				}
				return p.Payload, nil
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// A fixture bot's microphone.
	js := join("Sam", true)
	mic, err := fixturemeet.JoinMic(ctx, devjoin.RTC(js), "microphone")
	if err != nil {
		t.Fatal(err)
	}
	defer mic.Close()
	var samTalk talk
	go samTalk.run(ctx, mic, 's')

	// Ann talks for 3 s.
	ja := join("Ann", false)
	as, err := sfuclient.Join(ctx, ja.MediaURL, ja.Token, sfuclient.Options{SendOnly: true}, sfuclient.Events{})
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	annMic, _, err := as.PublishAudio(ctx, "microphone")
	if err != nil {
		t.Fatal(err)
	}
	var annTalk talk
	annCtx, annStop := context.WithTimeout(ctx, 3*time.Second)
	defer annStop()
	annTalk.run(annCtx, annMic, 'a')

	// Wait for the echo to finish.
	deadline := time.Now().Add(5 * time.Second)
	for last := -1; time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		n := bea.frames(je.Identity, 'a')
		if n > 0 && n == last {
			break
		}
		last = n
	}
	if n := mr.People(); n != 4 {
		t.Errorf("the whiteboard counts %d people, want 4 (Echo, Bea, Sam, Ann)", n)
	}
	stopEcho()
	if err := <-echoDone; err != nil {
		t.Errorf("the echo bot: %v", err)
	}

	// The echo bot repeated Ann, once, and nothing else.
	echoes := bea.from(je.Identity)
	if len(echoes) != 1 {
		t.Fatalf("Bea heard %d tracks from the echo bot, want 1 (Ann's)", len(echoes))
	}
	bea.mu.Lock()
	e := echoes[0]
	if !strings.Contains(e.name, ja.Identity) || e.tags['s'] > 0 || e.tags['w'] > 0 || len(e.tags) != 1 {
		t.Errorf("the echo track %q carried %v", e.name, e.tags)
	}
	var delays []time.Duration
	for i, n := range e.frames {
		if at, ok := annTalk.when(n); ok {
			delays = append(delays, e.at[i].Sub(at))
		}
	}
	bea.mu.Unlock()
	written := len(annTalk.at)
	if len(delays) < written*9/10 {
		t.Errorf("the echo carried %d of Ann's %d frames", len(delays), written)
	}
	slices.Sort(delays)
	if len(delays) > 0 {
		lo, med, hi := delays[0], delays[len(delays)/2], delays[len(delays)-1]
		t.Logf("the echo: %d of Ann's %d frames, delay median %v (%v–%v)", len(delays), written,
			med.Round(time.Millisecond), lo.Round(time.Millisecond), hi.Round(time.Millisecond))
		if med < 1450*time.Millisecond || med > 1600*time.Millisecond {
			t.Errorf("the echo came %v after Ann, want about 1.5 s", med)
		}
	}

	// The fixture bot was heard by the whiteboard and by Bea; Bea heard the
	// whiteboard and Ann.
	for _, c := range []struct {
		what string
		n    int
	}{
		{"the whiteboard hearing Sam", wb.frames(js.Identity, 's')},
		{"the whiteboard hearing Ann", wb.frames(ja.Identity, 'a')},
		{"Bea hearing Sam", bea.frames(js.Identity, 's')},
		{"Bea hearing Ann", bea.frames(ja.Identity, 'a')},
		{"Bea hearing the whiteboard", bea.frames(agent.Identity, 'w')},
	} {
		if c.n < 100 {
			t.Errorf("%s: %d frames", c.what, c.n)
		}
	}
	t.Logf("the whiteboard heard Sam %d, Ann %d; Bea heard Sam %d, Ann %d, the whiteboard %d",
		wb.frames(js.Identity, 's'), wb.frames(ja.Identity, 'a'), bea.frames(js.Identity, 's'), bea.frames(ja.Identity, 'a'), bea.frames(agent.Identity, 'w'))
	if err := mic.WriteFrame(tagged('s', 0), 0, nil); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("the fixture bot's microphone: %v", err)
	}
}

// writerFunc is a function as a FrameWriter.
type writerFunc func(frame []byte, ts uint32, level *uint8) error

func (f writerFunc) WriteFrame(frame []byte, ts uint32, level *uint8) error { return f(frame, ts, level) }

// TestEchoLive checks a running builtin server's echo bot (make bot
// ARGS=echo) from outside: a person talks for 3 s, and a listener hears the
// echo bot repeat it once, 1.5 s later. Against a running server only:
//
//	HUDDLE_ECHO_TEST=http://localhost:8080 HUDDLE_ECHO_ROOM=test go test ./cmd/huddled -run EchoLive -v
func TestEchoLive(t *testing.T) {
	server, room := os.Getenv("HUDDLE_ECHO_TEST"), os.Getenv("HUDDLE_ECHO_ROOM")
	if server == "" || room == "" {
		t.Skip("set HUDDLE_ECHO_TEST to a builtin server running an echo bot in HUDDLE_ECHO_ROOM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	join := func(name string) wire.Joined {
		j, err := devjoin.Join(ctx, server, wire.DevJoin{Room: room, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if j.Media != wire.MediaBuiltin {
			t.Fatalf("the server's media is %q", j.Media)
		}
		return j
	}
	bea := &listen{voices: map[string]*got{}}
	jb := join("Bea")
	bs, err := sfuclient.Join(ctx, jb.MediaURL, jb.Token, sfuclient.Options{}, sfuclient.Events{
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			go bea.read(rt.SID, rt.Participant, rt.Name, func() ([]byte, error) {
				p, _, err := rt.Track.ReadRTP()
				if err != nil {
					return nil, err
				}
				return p.Payload, nil
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()
	ja := join("Ann")
	as, err := sfuclient.Join(ctx, ja.MediaURL, ja.Token, sfuclient.Options{SendOnly: true}, sfuclient.Events{})
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	annMic, _, err := as.PublishAudio(ctx, "microphone")
	if err != nil {
		t.Fatal(err)
	}
	var annTalk talk
	annCtx, annStop := context.WithTimeout(ctx, 3*time.Second)
	defer annStop()
	annTalk.run(annCtx, annMic, 'a')
	time.Sleep(2500 * time.Millisecond)

	bea.mu.Lock()
	defer bea.mu.Unlock()
	var echoes []*got
	for _, g := range bea.voices {
		if g.tags['a'] > 0 && g.who != ja.Identity {
			echoes = append(echoes, g)
		}
	}
	if len(echoes) != 1 {
		t.Fatalf("%d tracks carried Ann's voice besides hers, want the echo bot's alone", len(echoes))
	}
	e := echoes[0]
	var delays []time.Duration
	for i, n := range e.frames {
		if at, ok := annTalk.when(n); ok {
			delays = append(delays, e.at[i].Sub(at))
		}
	}
	slices.Sort(delays)
	if len(delays) < len(annTalk.at)*9/10 {
		t.Fatalf("the echo carried %d of Ann's %d frames", len(delays), len(annTalk.at))
	}
	med := delays[len(delays)/2]
	t.Logf("%s's %q: %d of Ann's %d frames, delay median %v (%v–%v); %d voices heard in all",
		e.who, e.name, len(delays), len(annTalk.at), med.Round(time.Millisecond), delays[0].Round(time.Millisecond),
		delays[len(delays)-1].Round(time.Millisecond), len(bea.voices))
	if !strings.HasSuffix(e.who, wire.BotSuffix) || med < 1450*time.Millisecond || med > 1600*time.Millisecond {
		t.Errorf("the echo came from %s %v after Ann, want a bot about 1.5 s after", e.who, med)
	}
}
