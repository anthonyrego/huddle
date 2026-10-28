package agent

import (
	"context"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/wire"
)

// Manager runs one whiteboard per active room: started when someone joins
// the room or its WebSocket, gone once the room has been empty a while.
type Manager struct {
	Base Config // every agent's config; Room is set per agent

	mu     sync.Mutex
	ctx    context.Context
	agents map[string]*Agent
	wg     sync.WaitGroup
}

// NewManager runs agents until ctx ends.
func NewManager(ctx context.Context, base Config) *Manager {
	return &Manager{Base: base, ctx: ctx, agents: map[string]*Agent{}}
}

// Ensure starts the room's whiteboard if it isn't running.
func (m *Manager) Ensure(room string) *Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.agents[room]; a != nil {
		return a
	}
	if m.ctx.Err() != nil {
		return nil
	}
	cfg := m.Base
	cfg.Room = room
	a := New(cfg)
	m.agents[room] = a
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		start := time.Now()
		err := a.Run(m.ctx)
		if err != nil {
			a.log.Error("agent: stopped", "err", err, "after", time.Since(start).Round(time.Second))
		}
		m.mu.Lock()
		if m.agents[room] == a {
			delete(m.agents, room)
		}
		m.mu.Unlock()
	}()
	return a
}

// Deliver gives an app's frame to its room's whiteboard.
func (m *Manager) Deliver(room, identity string, e wire.Envelope) {
	if e.Kind != wire.KindCommand && e.Kind != wire.KindAddress {
		return
	}
	if a := m.Ensure(room); a != nil {
		a.Deliver(identity, e)
	}
}

// Wait waits for every agent to stop (after the context ends).
func (m *Manager) Wait() { m.wg.Wait() }
