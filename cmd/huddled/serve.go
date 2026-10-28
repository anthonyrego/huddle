package main

// serve: the HTTP API and the meeting WebSocket over Postgres, and the
// media server's part (media.go: HUDDLE_MEDIA, HUDDLE_NODE_IP,
// HUDDLE_RTC_SECRET). Stage 1 has no signing in, so it only runs in dev
// mode (HUDDLE_DEV=1).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/logx"
	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/server/wshub"
	"github.com/anthonyrego/huddle/internal/wire"
)

func init() {
	register(&command{name: "serve", summary: "run the API and the meeting WebSocket (stage 1: dev mode only)", run: runServe})
}

// env is an environment variable, or def when it's unset.
func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", env("HUDDLE_ADDR", ":8080"), "listen address (HUDDLE_ADDR)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Getenv("HUDDLE_DEV") != "1" {
		return errors.New("there's no signing in yet (stage 3), so the server only runs with HUDDLE_DEV=1, on a private network")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("set DATABASE_URL (make server does)")
	}
	// Logs go to stderr; a dev server (HUDDLE_DATA_DIR set, as make server
	// does) also keeps them in <data>/logs/serve.log, where agents read them.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if os.Getenv("HUDDLE_DATA_DIR") != "" {
		if paths, err := config.Resolve(); err == nil {
			if f, err := logx.Setup(paths.Logs, "serve", true); err == nil {
				defer f.Close()
			}
		}
	}
	md, err := newMedia(*addr)
	if err != nil {
		return err
	}
	defer md.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		slog.Info("serve: migrated", "versions", applied)
	}

	hub := &wshub.Hub{}
	// The whiteboard: one agent per active room, when there is a key.
	var agents *agent.Manager
	key := os.Getenv("GEMINI_API_KEY")
	switch {
	case key == "" || os.Getenv("HUDDLE_WHITEBOARD") == "0":
		slog.Warn("serve: no GEMINI_API_KEY: the whiteboard is off")
	default:
		agents = agent.NewManager(ctx, agent.Config{
			Store: st, Hub: hub, APIKey: key, Media: md.agent,
			Voice: os.Getenv("HUDDLE_VOICE"),
		})
		defer func() { stop(); agents.Wait() }()
	}
	hub.Join = func(ctx context.Context, hi wire.Hello) (wire.Welcome, [][]byte, error) {
		w, frames, err := welcome(ctx, st, hi)
		if err == nil && agents != nil {
			agents.Ensure(hi.Room)
		}
		return w, frames, err
	}
	hub.OnMessage = func(room, identity string, e wire.Envelope) {
		slog.Info("ws: message", "room", room, "identity", identity, "kind", e.Kind)
		if agents != nil {
			agents.Deliver(room, identity, e)
		}
	}
	api := &httpapi.Server{Rooms: storeRooms{st, agents}, WS: hub, Dev: true}
	md.api(api)
	srv := &http.Server{Addr: *addr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("serve: listening", append([]any{"addr", *addr, "dev", true}, md.attrs()...)...)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// welcome answers a hello with the room's head and, if the board has
// anything on it, that version.
func welcome(ctx context.Context, st *store.Store, hi wire.Hello) (wire.Welcome, [][]byte, error) {
	if _, err := st.EnsureMeeting(ctx, hi.Room); err != nil {
		return wire.Welcome{}, nil, fmt.Errorf("couldn't open the room: %w", err)
	}
	head, err := st.Head(ctx, hi.Room)
	if err != nil {
		return wire.Welcome{}, nil, err
	}
	w := wire.Welcome{Protocol: wire.Protocol, Room: hi.Room, Head: head}
	if head == 0 {
		return w, nil, nil
	}
	v, err := st.Version(ctx, hi.Room, head)
	if err != nil {
		return wire.Welcome{}, nil, err
	}
	frame, err := wire.Encode(wire.KindBoardVersion, wire.BoardVersion{
		Room: hi.Room, Seq: v.Seq, Parent: v.Parent, Requester: v.Requester, Command: v.Command,
		TMs: v.TMs, Doc: v.Doc, Layout: v.Layout,
	})
	if err != nil {
		return wire.Welcome{}, nil, err
	}
	return w, [][]byte{frame}, nil
}

// storeRooms makes dev rooms into meetings, with a whiteboard in each.
type storeRooms struct {
	st     *store.Store
	agents *agent.Manager
}

func (r storeRooms) EnsureRoom(ctx context.Context, room string) error {
	_, err := r.st.EnsureMeeting(ctx, room)
	if err == nil && r.agents != nil {
		r.agents.Ensure(room)
	}
	return err
}
