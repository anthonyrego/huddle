package main

// meet and type: dev tools against a running server. meet has a bot per
// speaker of a fixture meeting join a room and play its lines, for
// watching the whiteboard in the app; type sends a typed command to a
// room's whiteboard the way the app's command field does.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/fixturemeet"
	"github.com/anthonyrego/huddle/internal/wire"
)

func init() {
	register(&command{name: "meet", summary: "dev: fixture bots join a room on a running server and play a meeting", run: runMeet})
	register(&command{name: "type", summary: "dev: send a typed command to a room's whiteboard on a running server", run: runType})
}

func runMeet(args []string) error {
	fs := flag.NewFlagSet("meet", flag.ContinueOnError)
	server := fs.String("server", env("HUDDLE_SERVER", "http://localhost:8080"), "the server (HUDDLE_SERVER)")
	room := fs.String("room", "demo", "the room")
	meeting := fs.String("meeting", "testdata/meetings/review.txt", "the fixture script, rendered with make fixtures")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := fixturemeet.Parse(*meeting)
	if err != nil {
		return err
	}
	dir := filepath.Dir(*meeting)
	t, err := fixturemeet.ReadTiming(dir, s.Name)
	if err != nil {
		return err
	}
	var bots []*fixturemeet.Bot
	for _, sp := range s.Speakers() {
		ogg, _ := fixturemeet.Paths(dir, s.Name, sp)
		bots = append(bots, &fixturemeet.Bot{Name: sp, Ogg: ogg})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("%s: %d bots join %s on %s and play %s\n", s.Name, len(bots), *room, *server,
		(time.Duration(t.DurationMs) * time.Millisecond).Round(time.Second))
	return fixturemeet.PlayAll(ctx, *server, *room, bots, func(t0 time.Time) {
		go func() {
			for _, l := range t.Lines {
				at := t0.Add(time.Duration(l.StartMs) * time.Millisecond)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(at)):
				}
				label := ""
				if l.Label != fixturemeet.Talk {
					label = " [" + l.Label + "]"
				}
				fmt.Printf("%6s %s%s: %s\n", time.Since(t0).Round(time.Second), l.Speaker, label, l.Text)
			}
		}()
	})
}

func runType(args []string) error {
	fs := flag.NewFlagSet("type", flag.ContinueOnError)
	server := fs.String("server", env("HUDDLE_SERVER", "http://localhost:8080"), "the server (HUDDLE_SERVER)")
	room := fs.String("room", "demo", "the room")
	name := fs.String("name", "Typist", "who types it")
	wait := fs.Duration("wait", 8*time.Second, "how long to listen for what the whiteboard does")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return errors.New("usage: huddled type [-room R] words for the whiteboard")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+10*time.Second)
	defer cancel()
	j, err := devjoin.Join(ctx, *server, wire.DevJoin{Room: *room, Name: *name})
	if err != nil {
		return err
	}
	ws, _, err := websocket.Dial(ctx, j.WSURL, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	hello, _ := wire.Encode(wire.KindHello, wire.Hello{Protocol: wire.Protocol, Room: j.Room, Identity: j.Identity})
	cmd, _ := wire.Encode(wire.KindCommand, wire.Command{Room: j.Room, Text: text})
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		return err
	}
	if err := ws.Write(ctx, websocket.MessageText, cmd); err != nil {
		return err
	}
	sent := time.Now()
	fmt.Printf("sent %q to %s as %s\n", text, j.Room, j.Identity)
	rctx, rcancel := context.WithTimeout(ctx, *wait)
	defer rcancel()
	for {
		_, data, err := ws.Read(rctx)
		if err != nil {
			return nil
		}
		var e wire.Envelope
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		at := time.Since(sent).Round(time.Millisecond)
		switch e.Kind {
		case wire.KindAgentStatus:
			var s wire.AgentStatus
			e.Decode(&s)
			fmt.Printf("%8s status %s %s\n", at, s.State, s.Detail)
		case wire.KindBoardVersion:
			var v wire.BoardVersion
			e.Decode(&v)
			fmt.Printf("%8s version %d for %s (%s)\n", at, v.Seq, v.Requester, v.Command)
		case wire.KindBoardOps:
			var o wire.BoardOps
			e.Decode(&o)
			fmt.Printf("%8s ops for version %d: %d\n", at, o.Seq, len(o.Ops))
		}
	}
}
