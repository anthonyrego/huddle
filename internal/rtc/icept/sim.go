package icept

import (
	"io"
	"sync"
	"time"

	"github.com/pion/interceptor"

	"github.com/anthonyrego/huddle/internal/netsim"
)

// Sim is a pion interceptor that makes every remote stream's network worse
// on receive, through a netsim.Sim. It's registered ahead of the
// connection's own interceptors, so it sits next to the network: RTCP
// receiver reports, the jitter buffer and everything after see the
// simulated loss and delay, as they would a bad network.
type Sim struct {
	// New is called once per remote stream; a nil or inactive Sim leaves
	// that stream alone.
	New func() *netsim.Sim
}

// arrivedKey is the attribute under which Sim hands on when each packet
// came off the network.
type arrivedKey struct{}

// Arrived is when a packet read through a Sim came off the network, before
// the simulated delay: a tracing aid (huddle video -trace). It reads the
// attributes a track's ReadRTP returns; false without a Sim.
func Arrived(a interceptor.Attributes) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	t, ok := a.Get(arrivedKey{}).(time.Time)
	return t, ok
}

func (f Sim) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &simInterceptor{sim: f.New}, nil
}

type simInterceptor struct {
	interceptor.NoOp
	sim func() *netsim.Sim
	mu  sync.Mutex
	all []*simStream
}

// packet is one RTP packet copied off the wire.
type packet struct {
	b    []byte
	attr interceptor.Attributes
}

type simStream struct {
	in   interceptor.RTPReader
	out  chan packet
	pipe *netsim.Pipe[packet]
	err  chan error
	once sync.Once
	done chan struct{}
}

func (i *simInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	sim := i.sim()
	if sim == nil || !sim.Active() {
		return reader
	}
	s := &simStream{in: reader, out: make(chan packet, 512), err: make(chan error, 1), done: make(chan struct{})}
	s.pipe = netsim.NewPipe(sim, func(p packet) {
		select {
		case s.out <- p:
		default: // the reader fell far behind: as good as lost
		}
	})
	i.mu.Lock()
	i.all = append(i.all, s)
	i.mu.Unlock()
	go s.pump()
	return interceptor.RTPReaderFunc(func(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		select {
		case p := <-s.out:
			return copy(b, p.b), p.attr, nil
		case err := <-s.err:
			return 0, nil, err
		case <-s.done:
			return 0, nil, io.EOF
		}
	})
}

// pump reads the real stream into the simulated network.
func (s *simStream) pump() {
	buf := make([]byte, 1500)
	for {
		n, attr, err := s.in.Read(buf, interceptor.Attributes{})
		if err != nil {
			s.err <- err
			return
		}
		if attr == nil {
			attr = interceptor.Attributes{}
		}
		attr.Set(arrivedKey{}, time.Now())
		s.pipe.Send(packet{b: append([]byte(nil), buf[:n]...), attr: attr})
	}
}

func (s *simStream) close() {
	s.once.Do(func() {
		s.pipe.Close()
		close(s.done)
	})
}

func (i *simInterceptor) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, s := range i.all {
		s.close()
	}
	return nil
}
