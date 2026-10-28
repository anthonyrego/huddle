package main

// serve: the HTTP API and the meeting WebSocket over Postgres. Stage 1 has
// no signing in, so it only runs in dev mode (HUDDLE_DEV=1).

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

	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/lktoken"
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
	lkURL := os.Getenv("LIVEKIT_URL")
	minter := lktoken.Minter{Key: os.Getenv("LIVEKIT_API_KEY"), Secret: os.Getenv("LIVEKIT_API_SECRET"), TTL: 10 * time.Minute}
	if dbURL == "" || lkURL == "" || minter.Key == "" || minter.Secret == "" {
		return errors.New("set DATABASE_URL, LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET (make server does)")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

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

	hub := &wshub.Hub{
		Join: func(ctx context.Context, hi wire.Hello) (wire.Welcome, [][]byte, error) {
			return welcome(ctx, st, hi)
		},
		OnMessage: func(room, identity string, e wire.Envelope) {
			// Track A gives these to the room's whiteboard.
			slog.Info("ws: message", "room", room, "identity", identity, "kind", e.Kind)
		},
	}
	api := &httpapi.Server{Rooms: storeRooms{st}, Minter: minter, LiveKitURL: lkURL, WS: hub, Dev: true}
	srv := &http.Server{Addr: *addr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("serve: listening", "addr", *addr, "livekit", lkURL, "dev", true)
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

// storeRooms makes dev rooms into meetings.
type storeRooms struct{ st *store.Store }

func (r storeRooms) EnsureRoom(ctx context.Context, room string) error {
	_, err := r.st.EnsureMeeting(ctx, room)
	return err
}
