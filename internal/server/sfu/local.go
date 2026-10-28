package sfu

// The in-process participant: someone in a room with neither a WebSocket
// nor connections (the whiteboard, huddled's own). Everyone else sees an
// ordinary participant and its voice as an ordinary publication; it hears
// the people in the room and is handed each person's voice as RTP, through
// a down track whose packets go to a queue instead of a connection.

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// voiceQueue is how many packets a Voice holds for a reader that has
// fallen behind: 5 s of 20 ms frames. Past that the oldest go, so a slow
// reader never holds up the voice's publisher or the room.
const voiceQueue = 250

// ErrUnpublished is a write to a local track that is no longer published
// (unpublished, or its participant left).
var ErrUnpublished = errors.New("sfu: the track isn't published")

// LocalEvents are what an in-process participant hears of its room: the
// people in it (never an agent), and each person's voice as it's
// published. They come in order on one goroutine of the participant's
// own, never under the room's lock: a handler that blocks holds up only
// the events after it. A person arrives before their voice is handed
// over. None come after Leave.
type LocalEvents struct {
	// Arrived is a person in the room: everyone there at the join, then
	// each person who comes.
	Arrived func(identity, name string)
	// Left is a person gone.
	Left func(identity string)
	// Voice is a person's voice as it's published: audio only, never a
	// camera, a screen or an agent's voice. Read it until it ends, which
	// it does when its publisher unpublishes it or leaves (or the local
	// participant does).
	Voice func(identity, name string, v *Voice)
}

// Local is an in-process participant in one room.
type Local struct {
	p    *participant
	cids atomic.Int64
}

// LocalJoin puts an in-process participant in g.Room (made if it's the
// first one in), as g.Identity: others hear it arrive as they would anyone
// (Agent as the grant says). A session already there under the identity
// is ended with bye, as with a second join on /rtc.
func (s *Server) LocalJoin(g rtctoken.Grant, ev LocalEvents) (*Local, error) {
	if g.Room == "" || g.Identity == "" {
		return nil, errors.New("sfu: a local join needs a room and an identity")
	}
	p := &participant{
		s: s, grant: g, cancel: func() {},
		log:   s.log.With("room", g.Room, "identity", g.Identity),
		done:  make(chan struct{}),
		pubs:  map[string]*publication{}, byCID: map[string]*publication{}, downs: map[string]*downTrack{},
		gone:  true,
		local: &localPeer{ev: ev, events: newEventQueue(), people: map[string]bool{}},
	}
	go p.local.events.run()
	if !s.enter(p) {
		p.finish()
		return nil, errors.New("sfu: the server is closing")
	}
	return &Local{p: p}, nil
}

// PublishAudio publishes a voice named name, and returns the track to
// write its frames to and the SID the room knows it by. Everyone else
// hears the track as any voice, and is sent it.
func (l *Local) PublishAudio(name string) (*LocalTrack, string, error) {
	r := l.p.rm.Load()
	if r == nil {
		return nil, "", errors.New("sfu: not in a room")
	}
	t := &LocalTrack{ssrc: rand.Uint32(), seq: uint16(rand.Uint32())}
	pub, err := r.publish(l.p, signal.Publish{CID: fmt.Sprintf("local-%d", l.cids.Add(1)), Kind: signal.Audio, Source: signal.Microphone, Name: name}, t)
	if err != nil {
		return nil, "", fmt.Errorf("sfu: publish: %w", err)
	}
	t.pub = pub // before the caller has t, so before any frame
	return t, pub.sid, nil
}

// People is how many people (not agents) are in the room, the local
// participant aside.
func (l *Local) People() int {
	r := l.p.rm.Load()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.p.gone {
		return 0
	}
	n := 0
	for _, o := range r.order {
		if o != l.p && !o.grant.Agent {
			n++
		}
	}
	return n
}

// Leave takes the participant out of the room: its voice is unpublished
// (everyone hears untrack, then left), the voices it was handed end, and
// no more events come. It is safe to call more than once.
func (l *Local) Leave() { l.p.finish() }

// localPeer is what makes a participant in-process: the room's messages
// to it become LocalEvents, queued for its own goroutine.
type localPeer struct {
	ev     LocalEvents
	events *eventQueue

	mu     sync.Mutex
	people map[string]bool // told of (Arrived), by identity
}

// message takes what the room would send a client (participant.send),
// under the room's lock: it must not block. Who arrives and leaves
// becomes events; tracks don't (the room hands over voices through
// subscribe), nor does anything else.
func (lp *localPeer) message(body any) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	switch b := body.(type) {
	case signal.Joined:
		for _, o := range b.Participants {
			lp.arrivedLocked(o)
		}
	case signal.Participant:
		lp.arrivedLocked(b)
	case signal.Left:
		if lp.people[b.Identity] {
			delete(lp.people, b.Identity)
			lp.events.push(func() {
				if lp.ev.Left != nil {
					lp.ev.Left(b.Identity)
				}
			})
		}
	}
}

func (lp *localPeer) arrivedLocked(o signal.Participant) {
	if o.Agent || lp.people[o.Identity] {
		return
	}
	lp.people[o.Identity] = true
	lp.events.push(func() {
		if lp.ev.Arrived != nil {
			lp.ev.Arrived(o.Identity, o.Name)
		}
	})
}

// voice hands over a person's voice; the room's lock is held.
func (lp *localPeer) voice(pub *publication, v *Voice) {
	who, name := pub.owner.grant.Identity, pub.owner.grant.Name
	lp.events.push(func() {
		if lp.ev.Voice != nil {
			lp.ev.Voice(who, name, v)
		}
	})
}

// LocalTrack is a voice an in-process participant publishes. Its frames go
// to the publication as a remote voice's packets do, stamped with the
// track's SSRC and sequence numbers that run on from frame to frame. Each
// frame also stands as the publisher's sender report (RTP time ts is now),
// so down tracks send translated reports for it as for anyone's: the
// caller's timestamps follow its clock.
type LocalTrack struct {
	pub  *publication
	ssrc uint32

	mu  sync.Mutex
	seq uint16
}

// WriteFrame sends one Opus frame (20 ms) at RTP timestamp ts; level, when
// set, is its RFC 6464 level (-dBov, 0–127), which the room's speakers
// read. The frame is copied or written before it returns. It is
// ErrUnpublished once the track is.
func (t *LocalTrack) WriteFrame(frame []byte, ts uint32, level *uint8) error {
	now := time.Now()
	t.mu.Lock()
	t.seq++
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: codecs.OpusPT,
		SequenceNumber: t.seq, Timestamp: ts, SSRC: t.ssrc}, Payload: frame}
	t.mu.Unlock()
	t.pub.setReport(senderReport{ok: true, ssrc: t.ssrc, rtp: ts, at: now})
	var lv byte
	if level != nil {
		lv = *level
	}
	if !t.pub.forward(t, pkt, lv, level != nil, now) {
		return ErrUnpublished
	}
	return nil
}

// Voice is a person's voice as an in-process participant is handed it: the
// RTP packets the SFU forwards, numbered and timed as a down track's
// (sequence numbers unbroken, even when the publisher's stream starts
// again), each the Voice's own copy, without header extensions. It holds
// voiceQueue packets for a reader that falls behind, losing the oldest
// past that. It has the methods of a *webrtc.TrackRemote the whiteboard's
// ears read (agent.VoiceTrack); one goroutine reads it.
type Voice struct {
	codec webrtc.RTPCodecParameters
	wake  chan struct{}

	mu       sync.Mutex
	q        []*rtp.Packet // a ring of voiceQueue
	head, n  int
	deadline time.Time
	closed   bool
	dropped  int
}

func newVoice(codec webrtc.RTPCodecCapability) *Voice {
	return &Voice{
		codec: webrtc.RTPCodecParameters{RTPCodecCapability: codec, PayloadType: codecs.OpusPT},
		wake:  make(chan struct{}, 1),
		q:     make([]*rtp.Packet, voiceQueue),
	}
}

// put queues a packet, the oldest dropped when the queue is full. It never
// blocks.
func (v *Voice) put(p *rtp.Packet) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	if v.n == len(v.q) {
		v.q[v.head] = nil
		v.head = (v.head + 1) % len(v.q)
		v.n--
		v.dropped++
	}
	v.q[(v.head+v.n)%len(v.q)] = p
	v.n++
	v.mu.Unlock()
	v.signal()
}

func (v *Voice) signal() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

// close ends the voice: what's queued is still read, then io.EOF.
func (v *Voice) close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.signal()
}

// ReadRTP is the next packet, waiting for it until the read deadline (then
// os.ErrDeadlineExceeded, a net.Error whose Timeout is true) or the
// voice's end (io.EOF, once what was queued has been read).
func (v *Voice) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	for {
		v.mu.Lock()
		if v.n > 0 {
			p := v.q[v.head]
			v.q[v.head] = nil
			v.head = (v.head + 1) % len(v.q)
			v.n--
			v.mu.Unlock()
			return p, nil, nil
		}
		closed, deadline := v.closed, v.deadline
		v.mu.Unlock()
		if closed {
			return nil, nil, io.EOF
		}
		if deadline.IsZero() {
			<-v.wake
			continue
		}
		d := time.Until(deadline)
		if d <= 0 {
			return nil, nil, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		select {
		case <-v.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// SetReadDeadline bounds ReadRTP's wait, as a TrackRemote's does; the zero
// time waits for ever.
func (v *Voice) SetReadDeadline(t time.Time) error {
	v.mu.Lock()
	v.deadline = t
	v.mu.Unlock()
	v.signal()
	return nil
}

// Codec is the voice's: Opus.
func (v *Voice) Codec() webrtc.RTPCodecParameters { return v.codec }

// Dropped is how many packets were lost to a reader that fell behind.
func (v *Voice) Dropped() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.dropped
}

// eventQueue runs functions in order on one goroutine.
type eventQueue struct {
	mu      sync.Mutex
	items   []func()
	wake    chan struct{}
	stopped bool
}

func newEventQueue() *eventQueue { return &eventQueue{wake: make(chan struct{}, 1)} }

func (q *eventQueue) push(f func()) {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, f)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) run() {
	for {
		q.mu.Lock()
		if q.stopped {
			q.mu.Unlock()
			return
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			<-q.wake
			continue
		}
		f := q.items[0]
		q.items = slices.Delete(q.items, 0, 1)
		q.mu.Unlock()
		f()
	}
}

// stop runs nothing more.
func (q *eventQueue) stop() {
	q.mu.Lock()
	q.stopped, q.items = true, nil
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
