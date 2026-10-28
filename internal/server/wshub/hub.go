// Package wshub is the meeting WebSocket: each app connects, says hello
// for a room, and from then on hears that room's board versions, the
// whiteboard's status and the transcript. What apps send (typed commands,
// push-to-address) goes to OnMessage.
//
// A client that can't keep up is dropped rather than slowing the room; it
// reconnects and gets the head version again.
package wshub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/wire"
)

const (
	helloWait  = 10 * time.Second
	writeWait  = 10 * time.Second
	sendBuffer = 64
	maxFrame   = 64 << 10 // what an app may send in one frame
)

// Hub fans a room's messages out to its apps.
type Hub struct {
	// Join is called for a client's hello, before it hears anything else:
	// it returns the welcome and the frames that bring the client up to
	// date (the head version). An error refuses the client.
	Join func(ctx context.Context, h wire.Hello) (wire.Welcome, [][]byte, error)
	// OnMessage gets every frame an app sends after its hello.
	OnMessage func(room, identity string, e wire.Envelope)

	mu    sync.Mutex
	rooms map[string]map[*client]struct{}
}

type client struct {
	room, identity string
	send           chan []byte
	cancel         context.CancelFunc
}

// ServeHTTP upgrades the request and runs the client until it goes.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered
	}
	conn.SetReadLimit(maxFrame)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c, err := h.hello(ctx, conn)
	if err != nil {
		slog.Info("ws: refused", "err", err)
		if frame, eerr := wire.Encode(wire.KindError, wire.Error{Message: err.Error()}); eerr == nil {
			wctx, wcancel := context.WithTimeout(ctx, writeWait)
			conn.Write(wctx, websocket.MessageText, frame)
			wcancel()
		}
		conn.Close(websocket.StatusPolicyViolation, "refused")
		return
	}
	c.cancel = cancel
	defer h.remove(c)
	go h.write(ctx, conn, c)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		e, err := decode(data)
		if err != nil {
			slog.Info("ws: bad frame", "room", c.room, "identity", c.identity, "err", err)
			continue
		}
		if h.OnMessage != nil {
			h.OnMessage(c.room, c.identity, e)
		}
	}
}

// hello reads the first frame, checks it and registers the client with
// what it should hear first already queued.
func (h *Hub) hello(ctx context.Context, conn *websocket.Conn) (*client, error) {
	hctx, cancel := context.WithTimeout(ctx, helloWait)
	defer cancel()
	_, data, err := conn.Read(hctx)
	if err != nil {
		return nil, fmt.Errorf("no hello: %w", err)
	}
	e, err := decode(data)
	if err != nil {
		return nil, err
	}
	if e.Kind != wire.KindHello {
		return nil, fmt.Errorf("first frame is %q, want hello", e.Kind)
	}
	var hi wire.Hello
	if err := e.Decode(&hi); err != nil {
		return nil, err
	}
	if hi.Protocol != wire.Protocol {
		return nil, fmt.Errorf("this server speaks protocol %d and the app %d: update the app", wire.Protocol, hi.Protocol)
	}
	if hi.Room == "" || hi.Identity == "" {
		return nil, errors.New("hello needs a room and an identity")
	}
	welcome := wire.Welcome{Protocol: wire.Protocol, Room: hi.Room}
	var initial [][]byte
	if h.Join != nil {
		if welcome, initial, err = h.Join(ctx, hi); err != nil {
			return nil, err
		}
	}
	first, err := wire.Encode(wire.KindWelcome, welcome)
	if err != nil {
		return nil, err
	}
	c := &client{room: hi.Room, identity: hi.Identity, send: make(chan []byte, sendBuffer+len(initial)+1)}
	c.send <- first
	for _, f := range initial {
		c.send <- f
	}
	h.mu.Lock()
	if h.rooms == nil {
		h.rooms = map[string]map[*client]struct{}{}
	}
	if h.rooms[c.room] == nil {
		h.rooms[c.room] = map[*client]struct{}{}
	}
	h.rooms[c.room][c] = struct{}{}
	h.mu.Unlock()
	slog.Info("ws: joined", "room", c.room, "identity", c.identity)
	return c, nil
}

func (h *Hub) write(ctx context.Context, conn *websocket.Conn, c *client) {
	defer conn.CloseNow()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case frame := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, writeWait)
			err := conn.Write(wctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.rooms[c.room]; cs != nil {
		delete(cs, c)
		if len(cs) == 0 {
			delete(h.rooms, c.room)
		}
	}
	slog.Info("ws: left", "room", c.room, "identity", c.identity)
}

// Broadcast sends a frame to every app in the room. An app whose queue is
// full is dropped.
func (h *Hub) Broadcast(room string, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[room] {
		select {
		case c.send <- frame:
		default:
			slog.Warn("ws: dropping a slow client", "room", room, "identity", c.identity)
			c.cancel()
			delete(h.rooms[room], c)
		}
	}
}

// Send encodes a body and broadcasts it.
func (h *Hub) Send(room, kind string, body any) error {
	frame, err := wire.Encode(kind, body)
	if err != nil {
		return err
	}
	h.Broadcast(room, frame)
	return nil
}

// Count is how many apps are in the room.
func (h *Hub) Count(room string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms[room])
}

func decode(data []byte) (wire.Envelope, error) {
	var e wire.Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("frame isn't an envelope: %w", err)
	}
	return e, nil
}
