package wshub

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/wire"
)

func dial(t *testing.T, srv *httptest.Server, hello wire.Hello) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	frame, _ := wire.Encode(wire.KindHello, hello)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	return conn
}

func next(t *testing.T, conn *websocket.Conn) wire.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHelloWelcomeBroadcastAndCommands(t *testing.T) {
	got := make(chan string, 1)
	h := &Hub{
		Join: func(ctx context.Context, hi wire.Hello) (wire.Welcome, [][]byte, error) {
			v, _ := wire.Encode(wire.KindBoardVersion, wire.BoardVersion{Room: hi.Room, Seq: 3})
			return wire.Welcome{Protocol: wire.Protocol, Room: hi.Room, Head: 3}, [][]byte{v}, nil
		},
		OnMessage: func(room, identity string, e wire.Envelope) {
			var c wire.Command
			e.Decode(&c)
			got <- room + "/" + identity + ": " + c.Text
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	a := dial(t, srv, wire.Hello{Protocol: wire.Protocol, Room: "r", Identity: "sam"})
	if e := next(t, a); e.Kind != wire.KindWelcome {
		t.Fatalf("first frame %q", e.Kind)
	}
	if e := next(t, a); e.Kind != wire.KindBoardVersion {
		t.Fatalf("second frame %q", e.Kind)
	}
	b := dial(t, srv, wire.Hello{Protocol: wire.Protocol, Room: "r", Identity: "ana"})
	next(t, b)
	next(t, b)

	if err := h.Send("r", wire.KindAgentStatus, wire.AgentStatus{Room: "r", State: wire.AgentListening}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*websocket.Conn{a, b} {
		if e := next(t, c); e.Kind != wire.KindAgentStatus {
			t.Fatalf("broadcast came as %q", e.Kind)
		}
	}

	frame, _ := wire.Encode(wire.KindCommand, wire.Command{Room: "r", Text: "draw it"})
	a.Write(context.Background(), websocket.MessageText, frame)
	select {
	case s := <-got:
		if s != "r/sam: draw it" {
			t.Fatalf("command arrived as %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command never arrived")
	}
}

func TestOldProtocolIsRefused(t *testing.T) {
	srv := httptest.NewServer(&Hub{})
	defer srv.Close()
	c := dial(t, srv, wire.Hello{Protocol: wire.Protocol - 1, Room: "r", Identity: "sam"})
	e := next(t, c)
	if e.Kind != wire.KindError {
		t.Fatalf("got %q, want an error", e.Kind)
	}
	var m wire.Error
	e.Decode(&m)
	if !strings.Contains(m.Message, "update the app") {
		t.Fatalf("message %q", m.Message)
	}
}
