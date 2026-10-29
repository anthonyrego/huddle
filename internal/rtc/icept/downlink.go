package icept

import (
	"io"
	"sync"
	"time"

	"github.com/pion/interceptor"
)

// Downlink is a pion interceptor that makes the downlink narrow on purpose,
// for tests: every RTP packet a peer connection receives, whichever stream
// it's on, goes through one token bucket at a fixed rate, with a queue of a
// fixed number of packets in front of it (the tail is dropped when it's
// full), as a home router's downlink would be. It sits next to the network,
// ahead of the connection's own interceptors: each stream's packets are
// read off the network into the one queue as they come, and handed to the
// stream's reader as the link lets them through, so the TWCC feedback
// (after it) times each packet then, and the sender's congestion control
// sees the queue as delay and what's dropped as loss. RTCP isn't limited.
//
// A dropped packet has been through SRTP already, so its resend (on the
// stream itself, where there's no RTX) would read as a replay: the
// connection's replay protection must be off (sfuclient.Options).
type Downlink struct {
	BPS   int // bits a second
	Slots int // packets the queue holds (UplinkSlots)
	// Made, when set, is given each connection's link as it's made.
	Made func(*RecvLink)
}

func (f *Downlink) NewInterceptor(string) (interceptor.Interceptor, error) {
	l := &RecvLink{bps: float64(f.BPS), slots: f.Slots, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if l.slots <= 0 {
		l.slots = UplinkSlots
	}
	if f.Made != nil {
		f.Made(l)
	}
	go l.run()
	return l, nil
}

// RecvLink is one connection's narrow downlink.
type RecvLink struct {
	interceptor.NoOp
	slots int

	mu      sync.Mutex
	bps     float64 // 0: no limit
	q       []recvd
	budget  float64 // bytes that may go now
	last    time.Time
	dropped int
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// recvd is a packet waiting in the queue, and the stream it's for.
type recvd struct {
	packet
	s *recvStream
}

type recvStream struct {
	out chan packet
	err chan error
}

func (l *RecvLink) BindRemoteStream(_ *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	s := &recvStream{out: make(chan packet, 512), err: make(chan error, 1)}
	go l.pump(s, reader)
	return interceptor.RTPReaderFunc(func(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		select {
		case p := <-s.out:
			return copy(b, p.b), p.attr, nil
		case err := <-s.err:
			return 0, nil, err
		case <-l.done:
			return 0, nil, io.EOF
		}
	})
}

// pump reads one stream off the network into the queue.
func (l *RecvLink) pump(s *recvStream, r interceptor.RTPReader) {
	buf := make([]byte, 1500)
	for {
		n, attr, err := r.Read(buf, interceptor.Attributes{})
		if err != nil {
			s.err <- err
			return
		}
		if attr == nil {
			attr = interceptor.Attributes{}
		}
		l.mu.Lock()
		if len(l.q) >= l.slots {
			l.dropped++
			l.mu.Unlock()
			continue // lost on the way, as far as anyone can tell
		}
		l.q = append(l.q, recvd{packet{b: append([]byte(nil), buf[:n]...), attr: attr}, s})
		l.mu.Unlock()
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

// run lets packets through at the rate, each to its stream's reader.
func (l *RecvLink) run() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		l.mu.Lock()
		now := time.Now()
		if !l.last.IsZero() {
			l.budget += now.Sub(l.last).Seconds() * l.bps / 8
		}
		l.last = now
		l.budget = min(l.budget, 1500) // a link saves up no more than a packet
		var out []recvd
		for len(l.q) > 0 && (l.bps == 0 || l.budget >= float64(len(l.q[0].b))) {
			r := l.q[0]
			l.q = l.q[1:]
			if l.bps > 0 {
				l.budget -= float64(len(r.b))
			}
			out = append(out, r)
		}
		wait := time.Hour
		if len(l.q) > 0 {
			need := float64(len(l.q[0].b)) - l.budget
			wait = max(time.Duration(need*8/l.bps*float64(time.Second)), time.Millisecond)
		}
		l.mu.Unlock()
		for _, r := range out {
			select {
			case r.s.out <- r.packet:
			default: // the reader fell far behind: as good as lost
			}
		}
		t.Reset(wait)
		select {
		case <-l.done:
			return
		case <-l.wake:
		case <-t.C:
		}
	}
}

// SetRate changes the link's rate; 0 lifts the limit.
func (l *RecvLink) SetRate(bps int) {
	l.mu.Lock()
	l.bps = float64(bps)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Dropped is how many packets the queue had no room for.
func (l *RecvLink) Dropped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

func (l *RecvLink) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
