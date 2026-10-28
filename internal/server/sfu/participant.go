package sfu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// participant is one session on /rtc: a person in a room, with the
// connection they publish on (up) and the one they're sent everyone else's
// tracks on (down).
type participant struct {
	s      *Server
	grant  rtctoken.Grant
	all    bool // sent everyone else's tracks (subscribe all)
	log    *slog.Logger
	conn   *websocket.Conn
	cancel context.CancelFunc
	sendq  chan []byte // nil frame: close after what's queued
	done   chan struct{}

	up, down *transport

	endOnce    sync.Once
	finishOnce sync.Once

	// rm is the room it's in, set once it enters.
	rm atomic.Pointer[room]

	// Guarded by room.mu once the participant is in a room.
	pubs  map[string]*publication // what it publishes, by SID
	byCID map[string]*publication
	downs map[string]*downTrack // what it's sent, by the track's SID
	gone  bool                  // left the room (or never entered)
}

func newParticipant(s *Server, conn *websocket.Conn, g rtctoken.Grant, all bool, cancel context.CancelFunc) (*participant, error) {
	p := &participant{
		s: s, grant: g, all: all, conn: conn, cancel: cancel,
		log:   s.log.With("room", g.Room, "identity", g.Identity),
		sendq: make(chan []byte, sendBuffer),
		done:  make(chan struct{}),
		pubs:  map[string]*publication{}, byCID: map[string]*publication{}, downs: map[string]*downTrack{},
		gone: true,
	}
	var err error
	if p.up, err = newTransport(p, up); err != nil {
		return nil, err
	}
	if p.down, err = newTransport(p, down); err != nil {
		p.up.close()
		return nil, err
	}
	return p, nil
}

// info is what others are told of p (its tracks added by the room).
func (p *participant) info() signal.Participant {
	return signal.Participant{Identity: p.grant.Identity, Name: p.grant.Name, Agent: p.grant.Agent}
}

// send queues a message for the client. A client whose queue is full is
// dropped: it can't keep up, and a message skipped would leave it wrong.
func (p *participant) send(kind string, body any) {
	frame, err := signal.Marshal(kind, body)
	if err != nil {
		p.log.Error("sfu: a message", "kind", kind, "err", err)
		return
	}
	p.push(frame)
}

func (p *participant) push(frame []byte) {
	select {
	case <-p.done:
		return
	default:
	}
	select {
	case p.sendq <- frame:
	default:
		p.log.Warn("sfu: dropping a slow client")
		p.cancel()
	}
}

// kick ends the session with bye: the client hears why, then the socket
// closes.
func (p *participant) kick(reason string) {
	p.endOnce.Do(func() {
		p.send(signal.KindBye, signal.Bye{Reason: reason})
		p.push(nil)
	})
}

// writeLoop writes what's queued until the session ends.
func (p *participant) writeLoop(ctx context.Context) {
	defer p.conn.CloseNow()
	for {
		select {
		case <-ctx.Done():
			p.conn.Close(websocket.StatusNormalClosure, "")
			return
		case frame := <-p.sendq:
			if frame == nil {
				p.conn.Close(websocket.StatusNormalClosure, "bye")
				p.cancel()
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeWait)
			err := p.conn.Write(wctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				p.cancel()
				return
			}
		}
	}
}

// pingLoop drops a client that stops answering pings (a network gone
// quiet, which TCP alone takes minutes to notice).
func (p *participant) pingLoop(ctx context.Context) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, writeWait)
		err := p.conn.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				p.log.Info("sfu: no answer to a ping", "err", err)
			}
			p.cancel()
			return
		}
	}
}

// readLoop handles what the client sends until it goes.
func (p *participant) readLoop(ctx context.Context) {
	for {
		_, data, err := p.conn.Read(ctx)
		if err != nil {
			return
		}
		e, err := decode(data)
		if err != nil {
			p.log.Info("sfu: bad frame", "err", err)
			continue
		}
		if e.Kind == signal.KindLeave {
			p.log.Debug("sfu: leaving")
			p.cancel()
			return
		}
		if err := p.handle(e); err != nil {
			p.log.Info("sfu: can't", "kind", e.Kind, "err", err)
			p.send(signal.KindError, signal.Error{Message: fmt.Sprintf("%s: %v", e.Kind, err)})
		}
	}
}

// handle acts on one message from the client.
func (p *participant) handle(e signal.Envelope) error {
	r := p.rm.Load()
	if r == nil {
		return errors.New("not in a room")
	}
	switch e.Kind {
	case signal.KindOffer:
		var sd signal.SDP
		if err := e.Decode(&sd); err != nil {
			return err
		}
		if sd.PC != signal.Pub {
			return fmt.Errorf("an offer for %q: the client offers on %q only", sd.PC, signal.Pub)
		}
		return p.up.answer(sd.SDP)
	case signal.KindAnswer:
		var sd signal.SDP
		if err := e.Decode(&sd); err != nil {
			return err
		}
		if sd.PC != signal.Sub {
			return fmt.Errorf("an answer for %q: the client answers on %q only", sd.PC, signal.Sub)
		}
		return p.down.setAnswer(sd.SDP)
	case signal.KindICE:
		var c signal.ICE
		if err := e.Decode(&c); err != nil {
			return err
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex, UsernameFragment: c.UsernameFragment}
		switch c.PC {
		case signal.Pub:
			return p.up.addCandidate(init)
		case signal.Sub:
			return p.down.addCandidate(init)
		}
		return fmt.Errorf("no connection %q", c.PC)
	case signal.KindPublish:
		var req signal.Publish
		if err := e.Decode(&req); err != nil {
			return err
		}
		return r.publish(p, req)
	case signal.KindUnpublish:
		var req signal.Unpublish
		if err := e.Decode(&req); err != nil {
			return err
		}
		return r.unpublish(p, req.SID)
	case signal.KindMute:
		var req signal.Mute
		if err := e.Decode(&req); err != nil {
			return err
		}
		return r.mute(p, req.SID, req.Muted)
	case signal.KindSettings:
		return nil // video comes with step 3
	}
	return fmt.Errorf("unknown message")
}

// onTrack is the up connection's: a track the client published starts
// arriving (a video's layers one by one).
func (p *participant) onTrack(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	var pub *publication
	if r := p.rm.Load(); r != nil {
		r.mu.Lock()
		pub = p.byCID[tr.ID()]
		r.mu.Unlock()
	}
	if pub == nil {
		p.log.Warn("sfu: a track nobody published", "cid", tr.ID(), "rid", tr.RID())
		drain(tr, rcv)
		return
	}
	pub.receive(tr, rcv)
}

// finish is the end of the session: out of the room, connections closed.
func (p *participant) finish() {
	p.finishOnce.Do(func() {
		p.cancel()
		if r := p.rm.Load(); r != nil {
			r.leave(p)
		}
		p.up.close()
		p.down.close()
		close(p.done)
	})
}

func decode(data []byte) (signal.Envelope, error) {
	var e signal.Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("frame isn't an envelope: %w", err)
	}
	return e, nil
}
