// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Adapted from livekit/server-sdk-go v2.19.0 room.go, engine.go and
// signalclient.go; changed: huddle's JSON signalling (internal/wire/signal)
// in place of LiveKit's protobuf, no data channels, no resume or
// reconnection (the caller joins again), events delivered in order on one
// goroutine, remote tracks handed over as pion's.

package sfuclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Signalling.
const (
	writeWait   = 10 * time.Second
	pingEvery   = 15 * time.Second // a server that doesn't answer a ping in writeWait is gone
	publishWait = 10 * time.Second // for the server's published
	sendBuffer  = 256
	maxFrame    = 1 << 20
)

// Events are what a session tells its caller. They're called in order, on
// one goroutine of the session's own: hand work elsewhere rather than block
// in them. The first is Joined, which may come before Join returns. None
// come after Close.
type Events struct {
	// Joined is the room as the session found it: who the client is, and
	// everyone else with what they publish.
	Joined func(self signal.Participant, others []signal.Participant)
	// Arrived and Left are others coming and going.
	Arrived func(signal.Participant)
	Left    func(identity string)
	// TrackInfo is another's track published or changed (muted), as the
	// server tells it. If the session subscribes, TrackAdded follows once
	// the track is on the receive connection.
	TrackInfo func(signal.TrackInfo)
	// TrackAdded is a remote track ready to read, before its first packet;
	// TrackRemoved is it gone (unpublished, or its publisher left).
	TrackAdded   func(*RemoteTrack)
	TrackRemoved func(*RemoteTrack)
	// Speakers is who's talking, loudest first; empty when no one is.
	Speakers func([]signal.Speaker)
	// Connection is one of the two connections changing state.
	Connection func(pc signal.PC, state webrtc.PeerConnectionState)
	// Disconnected is the session ending without Close: the server said
	// bye, the signalling dropped, or a connection failed. Join again.
	Disconnected func(err error)
}

// RemoteTrack is another participant's track, as the session receives it.
// Its TrackInfo is as it was when the track was added (TrackInfo events
// bring changes). The caller reads the track's RTP from Track and its RTCP
// from Receiver (the sender reports, for lip sync; pion's interceptors act
// on RTCP as it's read). Receiver's parameters name the negotiated header
// extensions (the RFC 6464 audio level).
type RemoteTrack struct {
	signal.TrackInfo
	Track    *webrtc.TrackRemote
	Receiver *webrtc.RTPReceiver
}

// ErrBye is the server ending the session; errors.Is finds it in
// Disconnected's error.
var ErrBye = errors.New("the server ended the session")

// RefusedError is the server turning a join away (a bad or expired token,
// another protocol), with its reason; errors.As finds it in Join's error.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "the server refused the join: " + e.Reason }

// Session is one join of the SFU: the signalling WebSocket and the two
// connections, publishing on one and receiving on the other.
type Session struct {
	opts   Options
	ev     Events
	log    *slog.Logger
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	sendq  chan []byte // nil: close the socket after what's queued
	wrote  chan struct{}
	events *queue

	pub, sub *peer

	mu      sync.Mutex
	room    string
	self    signal.Participant
	order   []string                      // others, in the order they joined
	others  map[string]signal.Participant // by identity, without tracks
	tracks  map[string]*remote            // others' tracks, by SID
	waiting map[string]chan published     // publishes waiting for published, by CID
	local   map[string]*localTrack        // what we publish, by SID
	cids    int
	ended   bool
}

// remote is another's track: what the server said of it, and the remote
// track once it's on the receive connection.
type remote struct {
	info signal.TrackInfo
	rt   *RemoteTrack
}

type published struct {
	sid string
	err error
}

// Join connects to the SFU's /rtc at url with a token from the dev join,
// and makes the two connections. ctx bounds the join, not the session.
func Join(ctx context.Context, url, token string, o Options, ev Events) (*Session, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("sfuclient: %w", err)
	}
	conn.SetReadLimit(maxFrame)
	j, err := join(ctx, conn, token, o)
	if err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("sfuclient: %w", err)
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Session{
		opts: o, ev: ev, log: log, conn: conn,
		sendq: make(chan []byte, sendBuffer), wrote: make(chan struct{}), events: newQueue(),
		room: j.Room, self: j.Self, others: map[string]signal.Participant{}, tracks: map[string]*remote{},
		waiting: map[string]chan published{}, local: map[string]*localTrack{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, p := range j.Participants {
		s.addOther(p)
	}
	if err := s.connect(); err != nil {
		s.cancel()
		conn.CloseNow()
		return nil, fmt.Errorf("sfuclient: %w", err)
	}
	self, others := s.Self(), s.Participants()
	s.events.push(func() {
		if ev.Joined != nil {
			ev.Joined(self, others)
		}
	})
	go s.events.run()
	go s.writeLoop()
	go s.pingLoop()
	go s.readLoop()
	return s, nil
}

// join sends the join and reads the answer.
func join(ctx context.Context, conn *websocket.Conn, token string, o Options) (signal.Joined, error) {
	sub := signal.SubscribeAll
	if o.SendOnly {
		sub = signal.SubscribeNone
	}
	frame, err := signal.Marshal(signal.KindJoin, signal.Join{Protocol: signal.Protocol, Token: token, Subscribe: sub})
	if err != nil {
		return signal.Joined{}, err
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		return signal.Joined{}, fmt.Errorf("join: %w", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		return signal.Joined{}, fmt.Errorf("join: %w", err)
	}
	var e signal.Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return signal.Joined{}, fmt.Errorf("join: %w", err)
	}
	switch e.Kind {
	case signal.KindJoined:
		var j signal.Joined
		err := e.Decode(&j)
		return j, err
	case signal.KindError:
		var m signal.Error
		e.Decode(&m)
		return signal.Joined{}, &RefusedError{Reason: m.Message}
	}
	return signal.Joined{}, fmt.Errorf("join: the server answered %q", e.Kind)
}

// connect makes the two connections.
func (s *Session) connect() error {
	pubAPI, err := NewAPI(s.opts, true)
	if err != nil {
		return err
	}
	subAPI, err := NewAPI(s.opts, false)
	if err != nil {
		return err
	}
	if s.pub, err = newPeer(s, signal.Pub, pubAPI); err != nil {
		return err
	}
	if s.sub, err = newPeer(s, signal.Sub, subAPI); err != nil {
		s.pub.close()
		return err
	}
	s.sub.pc.OnTrack(s.onTrack)
	return nil
}

// Room is the room joined.
func (s *Session) Room() string { return s.room }

// Self is who the session joined as.
func (s *Session) Self() signal.Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.self
}

// Participants are everyone else in the room, with what they publish.
func (s *Session) Participants() []signal.Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := make([]signal.Participant, 0, len(s.order))
	for _, id := range s.order {
		p := s.others[id]
		for _, r := range s.tracks {
			if r.info.Participant == id {
				p.Tracks = append(p.Tracks, r.info)
			}
		}
		slices.SortFunc(p.Tracks, func(a, b signal.TrackInfo) int {
			if a.SID < b.SID {
				return -1
			}
			return 1
		})
		ps = append(ps, p)
	}
	return ps
}

// addOther puts a participant in the table, with its tracks; s.mu is held
// (or the session isn't running yet).
func (s *Session) addOther(p signal.Participant) bool {
	_, known := s.others[p.Identity]
	for _, t := range p.Tracks {
		s.tracks[t.SID] = &remote{info: t}
	}
	p.Tracks = nil
	s.others[p.Identity] = p
	if !known {
		s.order = append(s.order, p.Identity)
	}
	return !known
}

// readLoop handles what the server sends until the session ends.
func (s *Session) readLoop() {
	for {
		_, data, err := s.conn.Read(s.ctx)
		if err != nil {
			s.fail(fmt.Errorf("the signalling: %w", err))
			return
		}
		var e signal.Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			s.log.Warn("sfuclient: a bad frame", "err", err)
			continue
		}
		if err := s.handle(e); err != nil {
			s.log.Warn("sfuclient: can't", "kind", e.Kind, "err", err)
		}
	}
}

// handle acts on one message from the server.
func (s *Session) handle(e signal.Envelope) error {
	switch e.Kind {
	case signal.KindParticipant:
		var p signal.Participant
		if err := e.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		if s.addOther(p) {
			p := s.others[p.Identity]
			s.push(func() {
				if s.ev.Arrived != nil {
					s.ev.Arrived(p)
				}
			})
		}
		s.mu.Unlock()
	case signal.KindLeft:
		var l signal.Left
		if err := e.Decode(&l); err != nil {
			return err
		}
		s.mu.Lock()
		if _, ok := s.others[l.Identity]; ok {
			for sid, r := range s.tracks {
				if r.info.Participant == l.Identity {
					s.removeTrackLocked(sid)
				}
			}
			delete(s.others, l.Identity)
			s.order = slices.DeleteFunc(s.order, func(id string) bool { return id == l.Identity })
			s.push(func() {
				if s.ev.Left != nil {
					s.ev.Left(l.Identity)
				}
			})
		}
		s.mu.Unlock()
	case signal.KindTrack:
		var t signal.TrackInfo
		if err := e.Decode(&t); err != nil {
			return err
		}
		s.mu.Lock()
		if r := s.tracks[t.SID]; r != nil {
			r.info = t
		} else {
			s.tracks[t.SID] = &remote{info: t}
		}
		s.push(func() {
			if s.ev.TrackInfo != nil {
				s.ev.TrackInfo(t)
			}
		})
		s.mu.Unlock()
	case signal.KindUntrack:
		var u signal.Untrack
		if err := e.Decode(&u); err != nil {
			return err
		}
		s.mu.Lock()
		s.removeTrackLocked(u.SID)
		s.mu.Unlock()
	case signal.KindOffer:
		var sd signal.SDP
		if err := e.Decode(&sd); err != nil {
			return err
		}
		if sd.PC != signal.Sub {
			return fmt.Errorf("an offer for %q", sd.PC)
		}
		return s.sub.answer(sd.SDP)
	case signal.KindAnswer:
		var sd signal.SDP
		if err := e.Decode(&sd); err != nil {
			return err
		}
		if sd.PC != signal.Pub {
			return fmt.Errorf("an answer for %q", sd.PC)
		}
		return s.pub.setAnswer(sd.SDP)
	case signal.KindICE:
		var c signal.ICE
		if err := e.Decode(&c); err != nil {
			return err
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex, UsernameFragment: c.UsernameFragment}
		switch c.PC {
		case signal.Pub:
			return s.pub.addCandidate(init)
		case signal.Sub:
			return s.sub.addCandidate(init)
		}
		return fmt.Errorf("no connection %q", c.PC)
	case signal.KindPublished:
		var p signal.Published
		if err := e.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		ch := s.waiting[p.CID]
		if ch != nil {
			ch <- published{sid: p.SID}
			delete(s.waiting, p.CID)
		}
		s.mu.Unlock()
		if ch == nil {
			// Published after the publish was given up on.
			s.unpublishLate(p.CID, p.SID)
		}
	case signal.KindSpeakers:
		var sp signal.Speakers
		if err := e.Decode(&sp); err != nil {
			return err
		}
		s.push(func() {
			if s.ev.Speakers != nil {
				s.ev.Speakers(sp.Speakers)
			}
		})
	case signal.KindError:
		var m signal.Error
		if err := e.Decode(&m); err != nil {
			return err
		}
		s.log.Warn("sfuclient: the server says", "err", m.Message)
		// The server's errors answer requests; one waiting to be
		// published is the likeliest.
		s.mu.Lock()
		for cid, ch := range s.waiting {
			ch <- published{err: fmt.Errorf("the server: %s", m.Message)}
			delete(s.waiting, cid)
		}
		s.mu.Unlock()
	case signal.KindBye:
		var b signal.Bye
		if err := e.Decode(&b); err != nil {
			return err
		}
		s.fail(fmt.Errorf("%w: %s", ErrBye, b.Reason))
	}
	return nil
}

// removeTrackLocked forgets a remote track, telling the caller if it had
// been added; s.mu is held.
func (s *Session) removeTrackLocked(sid string) {
	r := s.tracks[sid]
	if r == nil {
		return
	}
	delete(s.tracks, sid)
	if rt := r.rt; rt != nil {
		s.push(func() {
			if s.ev.TrackRemoved != nil {
				s.ev.TrackRemoved(rt)
			}
		})
	}
}

// onTrack is the receive connection's: a track the server offered is on
// it, ready to read. The server named it with a track message first.
func (s *Session) onTrack(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.tracks[tr.ID()]
	if r == nil || r.rt != nil || s.ended {
		s.log.Debug("sfuclient: a track not expected", "sid", tr.ID(), "from", tr.StreamID())
		return
	}
	rt := &RemoteTrack{TrackInfo: r.info, Track: tr, Receiver: rcv}
	r.rt = rt
	s.push(func() {
		if s.ev.TrackAdded != nil {
			s.ev.TrackAdded(rt)
		}
	})
}

// push queues an event, unless the session has ended.
func (s *Session) push(f func()) { s.events.push(f) }

// send queues a message for the server. A server that stops reading ends
// the session.
func (s *Session) send(kind string, body any) error {
	frame, err := signal.Marshal(kind, body)
	if err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return errors.New("the session has ended")
	default:
	}
	select {
	case s.sendq <- frame:
		return nil
	default:
		err := errors.New("the server isn't reading")
		s.fail(err)
		return err
	}
}

// writeLoop writes what's queued until the session ends.
func (s *Session) writeLoop() {
	defer close(s.wrote)
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.sendq:
			if frame == nil {
				s.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			wctx, cancel := context.WithTimeout(s.ctx, writeWait)
			err := s.conn.Write(wctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				s.fail(fmt.Errorf("the signalling: %w", err))
				return
			}
		}
	}
}

// pingLoop ends a session whose server stops answering pings.
func (s *Session) pingLoop() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(s.ctx, writeWait)
		err := s.conn.Ping(pctx)
		cancel()
		if err != nil {
			s.fail(fmt.Errorf("no answer to a ping: %w", err))
			return
		}
	}
}

// fail ends the session for err and tells the caller (Disconnected), once,
// unless Close got there first.
func (s *Session) fail(err error) {
	if !s.end() {
		return
	}
	s.log.Info("sfuclient: disconnected", "room", s.room, "identity", s.self.Identity, "err", err)
	s.cancel()
	s.conn.CloseNow()
	go func() {
		s.pub.close()
		s.sub.close()
	}()
	s.events.finish(func() {
		if s.ev.Disconnected != nil {
			s.ev.Disconnected(err)
		}
	})
}

// end marks the session over, failing what waits on it; false if it was
// already.
func (s *Session) end() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.ended = true
	for cid, ch := range s.waiting {
		ch <- published{err: errors.New("the session has ended")}
		delete(s.waiting, cid)
	}
	return true
}

// Close leaves the room: the server hears leave, the connections close,
// and no more events come. It is safe to call more than once.
func (s *Session) Close() error {
	if !s.end() {
		return nil
	}
	s.events.stop()
	if s.send(signal.KindLeave, nil) == nil {
		select {
		case s.sendq <- nil:
		default:
		}
		select {
		case <-s.wrote:
		case <-time.After(writeWait):
		}
	}
	s.cancel()
	s.conn.CloseNow()
	s.pub.close()
	s.sub.close()
	return nil
}

// queue delivers events in order on one goroutine.
type queue struct {
	mu        sync.Mutex
	items     []func()
	wake      chan struct{}
	finishing bool // run what's queued, then stop
	stopped   bool // run nothing more
}

func newQueue() *queue { return &queue{wake: make(chan struct{}, 1)} }

func (q *queue) push(f func()) {
	q.mu.Lock()
	if q.finishing || q.stopped {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, f)
	q.mu.Unlock()
	q.signal()
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) run() {
	for {
		q.mu.Lock()
		if q.stopped {
			q.mu.Unlock()
			return
		}
		if len(q.items) == 0 {
			if q.finishing {
				q.mu.Unlock()
				return
			}
			q.mu.Unlock()
			<-q.wake
			continue
		}
		f := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		f()
	}
}

// finish queues a last event: what's queued runs, then f, then nothing.
func (q *queue) finish(f func()) {
	q.mu.Lock()
	if !q.finishing && !q.stopped {
		q.items = append(q.items, f)
		q.finishing = true
	}
	q.mu.Unlock()
	q.signal()
}

// stop runs nothing more.
func (q *queue) stop() {
	q.mu.Lock()
	q.stopped, q.items = true, nil
	q.mu.Unlock()
	q.signal()
}
