package main

// The meeting as the app knows it: what the connection hears, handed to
// the main thread. The connection's goroutines write it under the lock and
// wake the loop; the scene takes a copy when it wakes.

import (
	"sync"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Connection states for the status pill.
const (
	connOffline    = "Offline"
	connNoRoom     = "Not in a room"
	connJoining    = "Joining…"
	connConnected  = "Connected"
	connRetrying   = "Reconnecting…"
	connFixture    = "Fixture"
	connFixtureEnd = "Fixture done"
)

// meeting is the shared state. Every write bumps rev and calls wake.
type meeting struct {
	mu   sync.Mutex
	wake func()
	m    meetingState
}

// meetingState is one copy of what's known.
type meetingState struct {
	rev      int
	conn     string // connection state
	detail   string // why it's offline, or the server
	room     string
	identity string
	// The board: the version shown and the ops that rebuild it. reveal
	// holds the ops the newest version added, for the reveal animation
	// (nil when a whole board arrives at once, as on joining).
	seq    int64
	ops    []board.Op
	reveal []board.Op
	agent  wire.AgentStatus
	lines  []wire.Segment
}

func (mt *meeting) update(f func(s *meetingState)) {
	mt.mu.Lock()
	f(&mt.m)
	mt.m.rev++
	mt.mu.Unlock()
	if mt.wake != nil {
		mt.wake()
	}
}

// take returns the state if it changed since rev.
func (mt *meeting) take(rev int) (meetingState, bool) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.m.rev == rev {
		return meetingState{}, false
	}
	s := mt.m
	s.ops = append([]board.Op(nil), s.ops...)
	s.lines = append([]wire.Segment(nil), s.lines...)
	mt.m.reveal = nil // revealed once
	return s, true
}

func (mt *meeting) setConn(conn, detail string) {
	mt.update(func(s *meetingState) { s.conn, s.detail = conn, detail })
}

// setVersion shows a version: its whole snapshot, revealing newOps when
// they're what it added to the version shown.
func (mt *meeting) setVersion(seq int64, ops, newOps []board.Op) {
	mt.update(func(s *meetingState) {
		if seq <= s.seq {
			return // an older version arriving late
		}
		s.seq, s.ops, s.reveal = seq, ops, newOps
	})
}

// maxLines is how much of the transcript the app keeps.
const maxLines = 500

func (mt *meeting) addLine(g wire.Segment) {
	mt.update(func(s *meetingState) {
		// A line still being said is replaced as it grows.
		if n := len(s.lines); n > 0 && !s.lines[n-1].Final && s.lines[n-1].Identity == g.Identity {
			s.lines[n-1] = g
			return
		}
		s.lines = append(s.lines, g)
		if len(s.lines) > maxLines {
			s.lines = s.lines[len(s.lines)-maxLines:]
		}
	})
}
