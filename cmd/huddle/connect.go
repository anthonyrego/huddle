package main

// The dev connection: join a room over HTTP, then hold the meeting
// WebSocket open, reconnecting with backoff, and put what it hears into
// the meeting. Stage 3 replaces the dev join with signing in.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/wire"
)

// conn is the app's link to the server for one room.
type conn struct {
	server, room, name string
	mt                 *meeting
	joined             wire.Joined
	out                chan []byte // frames to send (commands)
	// onJoin, when set, gets each join's answer (the voice session joins
	// LiveKit with its token).
	onJoin func(wire.Joined)
}

func newConn(server, room, name string, mt *meeting) *conn {
	return &conn{server: strings.TrimRight(server, "/"), room: room, name: name, mt: mt, out: make(chan []byte, 16)}
}

// join asks the server to let us in and keeps what it answers.
func (c *conn) join(ctx context.Context) error {
	body, _ := json.Marshal(wire.DevJoin{Room: c.room, Name: c.name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server+"/dev/join", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e wire.Error
		json.NewDecoder(res.Body).Decode(&e)
		return fmt.Errorf("join: %s: %s", res.Status, e.Message)
	}
	if err := json.NewDecoder(res.Body).Decode(&c.joined); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	c.mt.update(func(s *meetingState) { s.room, s.identity = c.joined.Room, c.joined.Identity })
	slog.Info("conn: joined", "room", c.joined.Room, "identity", c.joined.Identity, "livekit", c.joined.LiveKitURL)
	if c.onJoin != nil {
		c.onJoin(c.joined)
	}
	return nil
}

// run joins and holds the WebSocket until ctx ends.
func (c *conn) run(ctx context.Context) {
	c.mt.setConn(connJoining, c.server)
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.join(ctx)
		if err == nil {
			err = c.session(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("conn: lost", "err", err, "retry", backoff)
		c.mt.setConn(connRetrying, err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session is one WebSocket connection: hello, then frames until it drops.
func (c *conn) session(ctx context.Context) error {
	ws, _, err := websocket.Dial(ctx, c.joined.WSURL, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20) // a whole board version
	hello, _ := wire.Encode(wire.KindHello, wire.Hello{Protocol: wire.Protocol, Room: c.joined.Room, Identity: c.joined.Identity})
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-sctx.Done():
				return
			case f := <-c.out:
				if err := ws.Write(sctx, websocket.MessageText, f); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		_, data, err := ws.Read(sctx)
		if err != nil {
			return err
		}
		var e wire.Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			slog.Warn("conn: bad frame", "err", err)
			continue
		}
		if err := c.handle(e); err != nil {
			return err
		}
	}
}

func (c *conn) handle(e wire.Envelope) error {
	switch e.Kind {
	case wire.KindWelcome:
		var w wire.Welcome
		if err := e.Decode(&w); err != nil {
			return err
		}
		c.mt.setConn(connConnected, c.server)
		slog.Info("conn: welcome", "room", w.Room, "head", w.Head)
	case wire.KindBoardVersion:
		var v wire.BoardVersion
		if err := e.Decode(&v); err != nil {
			return err
		}
		snap, err := board.UnmarshalSnapshot(v.Doc)
		if err != nil {
			return err
		}
		slog.Info("conn: version", "seq", v.Seq, "ops", len(snap.Ops), "requester", v.Requester)
		c.mt.setVersion(v.Seq, snap.Ops, nil)
	case wire.KindAgentStatus:
		var a wire.AgentStatus
		if err := e.Decode(&a); err != nil {
			return err
		}
		c.mt.update(func(s *meetingState) { s.agent = a })
	case wire.KindTranscript:
		var g wire.Segment
		if err := e.Decode(&g); err != nil {
			return err
		}
		c.mt.addLine(g)
	case wire.KindError:
		var m wire.Error
		e.Decode(&m)
		return errors.New(m.Message)
	default:
		slog.Info("conn: ignored", "kind", e.Kind)
	}
	return nil
}

// send queues a frame for the server; it reports false when the queue is
// full (the connection is down).
func (c *conn) send(kind string, body any) bool {
	f, err := wire.Encode(kind, body)
	if err != nil {
		return false
	}
	select {
	case c.out <- f:
		return true
	default:
		return false
	}
}
