package main

// The dev connection: join a room over HTTP, then hold the meeting
// WebSocket open, reconnecting with backoff, and put what it hears into
// the meeting. Stage 3 replaces the dev join with signing in.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/wire"
)

// conn is the app's link to the server for one room.
type conn struct {
	server, room, name string
	mt                 *meeting
	joined             wire.Joined
	out                chan []byte // frames to send (commands)
	// onJoin, when set, gets each join's answer (the voice session joins
	// the media server with its token).
	onJoin func(wire.Joined)

	redo atomic.Bool // rejoin asked: join again without waiting
	mu   sync.Mutex
	drop context.CancelFunc // ends the current session
}

func newConn(server, room, name string, mt *meeting) *conn {
	return &conn{server: strings.TrimRight(server, "/"), room: room, name: name, mt: mt, out: make(chan []byte, 16)}
}

// join asks the server to let us in and keeps what it answers.
func (c *conn) join(ctx context.Context) error {
	j, err := devjoin.Join(ctx, c.server, wire.DevJoin{Room: c.room, Name: c.name})
	if err != nil {
		return err
	}
	c.joined = j
	c.redo.Store(false) // a fresh token is what a rejoin asks for
	c.mt.update(func(s *meetingState) { s.room, s.identity = c.joined.Room, c.joined.Identity })
	slog.Info("conn: joined", "room", j.Room, "identity", j.Identity, "mediaURL", j.MediaURL)
	if c.onJoin != nil {
		c.onJoin(c.joined)
	}
	return nil
}

// run joins and holds the WebSocket until ctx ends. While it can't, the
// pill says why (no answer, or the server's refusal, or a dropped
// connection) and counts down to the next try.
func (c *conn) run(ctx context.Context) {
	host := c.server
	if u, err := url.Parse(c.server); err == nil && u.Host != "" {
		host = u.Host
	}
	c.mt.update(func(s *meetingState) { s.conn, s.detail, s.server, s.room = connJoining, c.server, host, c.room })
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.join(ctx)
		joined := err == nil
		if joined {
			err = c.session(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		if joined && c.redo.Swap(false) {
			slog.Info("conn: joining again", "why", "voice asked for a new token")
			continue
		}
		state := connRetrying
		switch {
		case !joined && unreachable(err):
			state = connUnreachable
		case !joined:
			state = connRefused
		case time.Since(start) > 30*time.Second:
			backoff = time.Second // it was up a while: the drop is news
		}
		slog.Warn("conn: lost", "err", err, "retry", backoff)
		if !countdown(ctx, backoff, func(left int) {
			c.mt.update(func(s *meetingState) { s.conn, s.detail, s.retryIn = state, err.Error(), left })
		}) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// rejoin drops the meeting connection and joins again at once, for a
// fresh token (voice's has expired); the new identity is the WebSocket's
// and the voice's both.
func (c *conn) rejoin() {
	c.redo.Store(true)
	c.mu.Lock()
	drop := c.drop
	c.mu.Unlock()
	if drop != nil {
		drop()
	}
}

// unreachable is whether a join failed without an answer from the server
// (the address is wrong, or nothing is listening).
func unreachable(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}

// countdown waits d, telling tick the whole seconds left each second; it
// reports false if ctx ended first.
func countdown(ctx context.Context, d time.Duration, tick func(left int)) bool {
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return true
		}
		tick(int((left + time.Second - 1) / time.Second))
		step := left % time.Second
		if step == 0 {
			step = time.Second
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(step):
		}
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
	c.mu.Lock()
	c.drop = cancel
	c.mu.Unlock()
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
		var pl *layout.Placement
		if len(v.Layout) > 0 {
			pl = new(layout.Placement)
			if err := json.Unmarshal(v.Layout, pl); err != nil {
				slog.Warn("conn: layout", "err", err)
				pl = nil
			}
		}
		slog.Info("conn: version", "seq", v.Seq, "ops", len(snap.Ops), "requester", v.Requester, "placed", pl != nil)
		c.mt.setVersion(v.Seq, snap.Ops, pl, nil)
	case wire.KindBoardOps:
		var o wire.BoardOps
		if err := e.Decode(&o); err != nil {
			return err
		}
		var ops []board.Op
		for _, raw := range o.Ops {
			if op, err := board.ParseOp(raw); err == nil {
				ops = append(ops, op)
			}
		}
		c.mt.setOps(o.Seq, ops)
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
