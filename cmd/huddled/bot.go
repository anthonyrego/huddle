package main

// bot: dev participants that join a room through a running server's dev
// join (-server, as make bot passes it), as bots, and connect to the media
// server it names:
//
//   - echo repeats each person's voice 1.5 s later, as its own track, so
//     one person can test alone: you hear yourself once, and never an echo
//     of the echo if echo cancellation works (its own tracks, other bots'
//     and agents' aren't repeated: a bot's identity ends in wire.BotSuffix).
//   - play is a fixture participant: it plays an Ogg/Opus file, looping.
//   - cam publishes the test pattern as a camera (1280x720 at 30 fps, three
//     layers), or with -share as a screen (1920x1080 at 15 fps, two), so
//     one person can see video arrive alone (macOS: it encodes with
//     VideoToolbox).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/sdp/v3"

	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/fixturemeet"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/wire"
	sig "github.com/anthonyrego/huddle/internal/wire/signal"
)

func init() {
	register(&command{name: "bot", summary: "dev participants: bot echo (repeats each voice 1.5 s later), bot play -ogg f.ogg, bot cam [-share] (the test pattern)", run: runBot})
}

func runBot(args []string) error {
	if len(args) == 0 || (args[0] != "echo" && args[0] != "play" && args[0] != "cam") {
		return errors.New("usage: huddled bot echo [-room R] [-delay 1.5s] | huddled bot play -ogg f.ogg [-name N] [-room R] [-loop=false] | huddled bot cam [-name N] [-room R] [-share]; each takes -server URL")
	}
	kind := args[0]
	fs := flag.NewFlagSet("bot "+kind, flag.ContinueOnError)
	server := fs.String("server", env("HUDDLE_SERVER", "http://localhost:8080"), "the server whose dev join lets the bot in (HUDDLE_SERVER)")
	room := fs.String("room", "test", "the room")
	name := fs.String("name", "", "the name shown (default Echo, or the file's name)")
	delay := fs.Duration("delay", 1500*time.Millisecond, "echo: how much later each voice comes back")
	oggPath := fs.String("ogg", "", "play: the Ogg/Opus file (scripts/voice-fixtures.sh makes some)")
	loop := fs.Bool("loop", true, "play: start again at the end")
	share := fs.Bool("share", false, "cam: publish the pattern as a screen share")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if *name == "" {
		*name = "Echo"
		if kind == "cam" {
			*name = "Cam"
			if *share {
				*name = "Screen"
			}
		}
		if kind == "play" {
			base := strings.TrimSuffix(filepath.Base(*oggPath), filepath.Ext(*oggPath))
			*name = strings.ToUpper(base[:min(1, len(base))]) + base[min(1, len(base)):]
		}
	}
	if kind == "play" && *oggPath == "" {
		return errors.New("bot play needs -ogg")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	j, err := devjoin.Join(ctx, *server, wire.DevJoin{Room: *room, Name: *name, Bot: true})
	if err != nil {
		return err
	}
	m := devjoin.RTC(j)
	slog.Info("bot: let in", "room", j.Room, "identity", j.Identity, "mediaURL", m.URL)
	switch kind {
	case "echo":
		return runEcho(ctx, m, *delay, sfuclient.Options{})
	case "cam":
		return runCam(ctx, m, *share)
	default:
		return runPlay(ctx, m, *oggPath, *loop)
	}
}

// echoes reports whether the echo bot repeats a participant: people only,
// never a bot (the echo bot itself, fixture bots) or an agent (the
// whiteboard).
func echoes(identity string, agent bool) bool {
	return !agent && !strings.HasSuffix(identity, wire.BotSuffix)
}

// echo is the echo bot on huddled's own SFU: for each person's voice, a
// track of its own that repeats it, packet by packet, delay later.
type echo struct {
	ctx   context.Context
	delay time.Duration
	ready chan struct{} // s is set
	s     *sfuclient.Session

	mu     sync.Mutex
	agents map[string]bool // identities that are agents
}

// runEcho joins as the echo bot and echoes until ctx ends (or the session
// does: the error says why). o tunes the session (tests).
func runEcho(ctx context.Context, m rtc.Join, delay time.Duration, o sfuclient.Options) error {
	e := &echo{ctx: ctx, delay: delay, ready: make(chan struct{}), agents: map[string]bool{}}
	ended := make(chan error, 1)
	s, err := sfuclient.Join(ctx, m.URL, m.Token, o, sfuclient.Events{
		Joined: func(_ sig.Participant, others []sig.Participant) {
			for _, p := range others {
				e.seen(p)
			}
		},
		Arrived: e.seen,
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			e.mu.Lock()
			agent := e.agents[rt.Participant]
			e.mu.Unlock()
			if rt.Kind == sig.Audio && echoes(rt.Participant, agent) {
				go e.echo(rt)
			}
		},
		Disconnected: func(err error) { ended <- err },
	})
	if err != nil {
		return err
	}
	defer s.Close()
	e.s = s
	close(e.ready)
	slog.Info("bot: echo joined", "room", s.Room(), "delay", delay)
	select {
	case <-ctx.Done():
		return nil
	case err := <-ended:
		return fmt.Errorf("the media server: %w", err)
	}
}

func (e *echo) seen(p sig.Participant) {
	e.mu.Lock()
	e.agents[p.Identity] = p.Agent
	e.mu.Unlock()
}

// echo repeats one person's voice until it ends, then unpublishes the
// repeat.
func (e *echo) echo(rt *sfuclient.RemoteTrack) {
	<-e.ready
	who := rt.Participant
	track, sid, err := e.s.PublishAudio(e.ctx, "echo of "+who)
	if err != nil {
		slog.Error("bot: echo publish", "for", who, "err", err)
		return
	}
	slog.Info("bot: echoing", "identity", who, "track", rt.SID, "as", sid)
	var levelID uint8
	for _, x := range rt.Receiver.GetParameters().HeaderExtensions {
		if x.URI == sdp.AudioLevelURI {
			levelID = uint8(x.ID)
		}
	}
	type held struct {
		frame []byte
		ts    uint32
		level *uint8
		at    time.Time
	}
	q := make(chan held, 4096) // 80 s of 20 ms packets
	go func() {
		defer close(q)
		for {
			p, _, err := rt.Track.ReadRTP()
			if err != nil {
				return
			}
			h := held{frame: p.Payload, ts: p.Timestamp, at: time.Now().Add(e.delay)}
			if b := p.GetExtension(levelID); levelID != 0 && len(b) > 0 {
				lv := b[0] & 0x7f
				h.level = &lv
			}
			select {
			case q <- h:
			default:
			}
		}
	}()
	for h := range q {
		if d := time.Until(h.at); d > 0 {
			time.Sleep(d)
		}
		track.WriteFrame(h.frame, h.ts, h.level) // before it's negotiated: dropped
	}
	e.s.Unpublish(sid)
	slog.Info("bot: stopped echoing", "identity", who)
}

// runPlay plays an Ogg/Opus file as a participant's mic, in real time,
// with each packet's RFC 6464 level.
func runPlay(ctx context.Context, m rtc.Join, path string, loop bool) error {
	t, err := fixturemeet.ReadTrack(path)
	if err != nil {
		return err
	}
	mic, err := fixturemeet.JoinMic(ctx, m, "microphone")
	if err != nil {
		return err
	}
	defer mic.Close()
	slog.Info("bot: playing", "file", path, "packets", t.Packets(), "loop", loop)
	if err := t.Play(ctx, mic, time.Now(), loop); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
