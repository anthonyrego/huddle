package lkrtc

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// uplinkFactory is a pion interceptor that makes the uplink narrow on
// purpose, for tests: every RTP packet a peer connection sends goes
// through one token bucket at a fixed rate, with a queue of a fixed number
// of packets in front of it (the tail is dropped when it's full), as a
// home router's uplink would be (50 packets is dummynet's default
// queue). It sits next to the network, after the pacer and the congestion
// control, so the SFU sees the queueing as delay and the estimator hears
// about it through the feedback, as it would from a real narrow link.
type uplinkFactory struct {
	bps   int // bits a second
	slots int // packets the queue holds
	made  func(*uplink)
}

// uplinkSlots is the queue in front of the limit, in packets.
const uplinkSlots = 50

func (f *uplinkFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	u := &uplink{bps: float64(f.bps), slots: f.slots, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if f.made != nil {
		f.made(u)
	}
	go u.run()
	return u, nil
}

type uplink struct {
	interceptor.NoOp
	bps   float64
	slots int

	mu      sync.Mutex
	q       []shaped
	budget  float64 // bytes that may go now
	last    time.Time
	dropped int
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
}

type shaped struct {
	h    rtp.Header
	p    []byte
	attr interceptor.Attributes
	w    interceptor.RTPWriter
}

func (u *uplink) BindLocalStream(_ *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, attr interceptor.Attributes) (int, error) {
		n := h.MarshalSize() + len(payload)
		u.mu.Lock()
		if len(u.q) >= u.slots {
			u.dropped++
			u.mu.Unlock()
			return n, nil // lost on the way, as far as anyone can tell
		}
		u.q = append(u.q, shaped{h: h.Clone(), p: append([]byte(nil), payload...), attr: attr, w: w})
		u.mu.Unlock()
		select {
		case u.wake <- struct{}{}:
		default:
		}
		return n, nil
	})
}

// run lets packets through at the rate.
func (u *uplink) run() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		u.mu.Lock()
		now := time.Now()
		if !u.last.IsZero() {
			u.budget += now.Sub(u.last).Seconds() * u.bps / 8
		}
		u.last = now
		u.budget = min(u.budget, 1500) // a link saves up no more than a packet
		var out []shaped
		for len(u.q) > 0 && u.budget >= float64(u.q[0].h.MarshalSize()+len(u.q[0].p)) {
			s := u.q[0]
			u.q = u.q[1:]
			u.budget -= float64(s.h.MarshalSize() + len(s.p))
			out = append(out, s)
		}
		wait := time.Hour
		if len(u.q) > 0 {
			need := float64(u.q[0].h.MarshalSize()+len(u.q[0].p)) - u.budget
			wait = max(time.Duration(need*8/u.bps*float64(time.Second)), time.Millisecond)
		}
		u.mu.Unlock()
		for _, s := range out {
			s.w.Write(&s.h, s.p, s.attr)
		}
		t.Reset(wait)
		select {
		case <-u.done:
			return
		case <-u.wake:
		case <-t.C:
		}
	}
}

// Dropped is how many packets the queue had no room for.
func (u *uplink) Dropped() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dropped
}

func (u *uplink) Close() error {
	u.once.Do(func() { close(u.done) })
	return nil
}
