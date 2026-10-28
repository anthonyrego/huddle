// Package netsim makes a network worse on purpose: it drops packets,
// delays them by a random amount and reorders some, so the voice path can
// be tested for loss and jitter on one Mac. Sim decides each packet's fate
// (pure and seedable, for scripted tests); Pipe applies it in real time.
package netsim

import (
	"container/heap"
	"math/rand/v2"
	"sync"
	"time"
)

// Sim is what to do to packets.
type Sim struct {
	// Loss is the chance a packet never arrives.
	Loss float64
	// Jitter J holds each packet an extra random delay, uniform between 0
	// and 2J (±J around J, as netem's "delay J J" does). Packets keep their
	// order (a packet waits for the one before it), as on most paths,
	// unless Reorder picks them.
	Jitter time.Duration
	// Reorder is the chance a packet is held back so that the next one
	// overtakes it.
	Reorder float64

	rng  *rand.Rand
	last time.Time // when the previous in-order packet is delivered
}

// New returns a Sim with its own random numbers from seed.
func New(loss float64, jitter time.Duration, reorder float64, seed uint64) *Sim {
	return &Sim{Loss: loss, Jitter: jitter, Reorder: reorder, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Active reports whether the Sim changes anything.
func (s *Sim) Active() bool { return s != nil && (s.Loss > 0 || s.Jitter > 0 || s.Reorder > 0) }

// Fate decides what happens to a packet that arrives at t: it is delivered
// at the returned time, or dropped.
func (s *Sim) Fate(t time.Time) (deliver time.Time, drop bool) {
	if s.rng == nil {
		s.rng = rand.New(rand.NewPCG(1, 2))
	}
	if s.Loss > 0 && s.rng.Float64() < s.Loss {
		return time.Time{}, true
	}
	at := t
	if s.Jitter > 0 {
		at = at.Add(time.Duration(s.rng.Int64N(int64(2*s.Jitter) + 1)))
	}
	if s.Reorder > 0 && s.rng.Float64() < s.Reorder {
		// Held past the next packet (20 ms behind) without holding it up.
		return at.Add(30 * time.Millisecond), false
	}
	if at.Before(s.last) {
		at = s.last
	}
	s.last = at
	return at, false
}

// Pipe delivers what it is given through a Sim, in real time: each packet
// is passed to the deliver func when its time comes, on Pipe's own
// goroutine.
type Pipe[T any] struct {
	sim     *Sim
	deliver func(T)
	mu      sync.Mutex
	q       queue[T]
	seq     int64
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// NewPipe starts a pipe. Close it when done.
func NewPipe[T any](sim *Sim, deliver func(T)) *Pipe[T] {
	p := &Pipe[T]{sim: sim, deliver: deliver, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go p.run()
	return p
}

// Send puts v into the network now.
func (p *Pipe[T]) Send(v T) {
	p.mu.Lock()
	at, drop := p.sim.Fate(time.Now())
	if drop {
		p.mu.Unlock()
		return
	}
	p.seq++
	heap.Push(&p.q, item[T]{at: at, seq: p.seq, v: v})
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Close stops delivering; what is still in flight is dropped.
func (p *Pipe[T]) Close() { p.once.Do(func() { close(p.done) }) }

func (p *Pipe[T]) run() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		p.mu.Lock()
		var due []T
		now := time.Now()
		for p.q.Len() > 0 && !p.q[0].at.After(now) {
			due = append(due, heap.Pop(&p.q).(item[T]).v)
		}
		wait := time.Hour
		if p.q.Len() > 0 {
			wait = p.q[0].at.Sub(now)
		}
		p.mu.Unlock()
		for _, v := range due {
			p.deliver(v)
		}
		t.Reset(wait)
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-t.C:
		}
	}
}

type item[T any] struct {
	at  time.Time
	seq int64
	v   T
}

type queue[T any] []item[T]

func (q queue[T]) Len() int { return len(q) }
func (q queue[T]) Less(i, j int) bool {
	if q[i].at.Equal(q[j].at) {
		return q[i].seq < q[j].seq
	}
	return q[i].at.Before(q[j].at)
}
func (q queue[T]) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *queue[T]) Push(x any)   { *q = append(*q, x.(item[T])) }
func (q *queue[T]) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}
