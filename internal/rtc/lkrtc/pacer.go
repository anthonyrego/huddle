package lkrtc

import (
	"container/list"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtp"
)

// The send side's congestion control: pion's GCC estimator (delay and loss
// based, fed by the SFU's transport-wide feedback), with a pacer of our
// own. pion's pacer queues every packet in one line, so the voice waits
// behind a keyframe; ours sends audio the moment it's written and paces
// video at the estimate. It also knows a stream's retransmissions (RTX),
// which reach it under their own SSRC.

// Pacing.
const (
	// paceFactor is how far over the estimate video may be sent while a
	// queue drains (pion's is 1.5, WebRTC's 2.5). Kept low: whatever goes
	// out faster than the narrowest link queues there, in front of the
	// voice.
	paceFactor = 1.2
	// paceBurst is how much sending time the pacer may save up while idle.
	paceBurst = 10 * time.Millisecond
	// paceTick is how often the pacer wakes while it holds packets.
	paceTick = 5 * time.Millisecond
)

// pacer is a gcc.Pacer: AddStream registers a stream's writer, Write
// sends or queues one packet.
type pacer struct {
	mu      sync.Mutex
	writers map[uint32]interceptor.RTPWriter
	audio   map[uint32]bool
	alias   map[uint32]uint32 // a retransmission SSRC → its stream's
	rate    float64           // bits a second video is sent at
	budget  float64           // bytes that may go now
	last    time.Time
	q       list.List // of *paced
	qBytes  int
	sent    rateMeter // everything sent, for the estimate's cap
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
}

type paced struct {
	h    rtp.Header
	p    []byte
	attr interceptor.Attributes
	w    interceptor.RTPWriter
	at   time.Time
}

func newPacer(initial int) *pacer {
	p := &pacer{
		writers: map[uint32]interceptor.RTPWriter{},
		audio:   map[uint32]bool{},
		alias:   map[uint32]uint32{},
		rate:    paceFactor * float64(initial),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go p.run()
	return p
}

// AddStream is how gcc registers a stream (with the writer that records
// it for the feedback).
func (p *pacer) AddStream(ssrc uint32, w interceptor.RTPWriter) {
	p.mu.Lock()
	p.writers[ssrc] = w
	p.mu.Unlock()
}

// stream tells the pacer what a stream is before gcc adds it.
func (p *pacer) stream(info *interceptor.StreamInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(strings.ToLower(info.MimeType), "audio/") {
		p.audio[info.SSRC] = true
	}
	if info.SSRCRetransmission != 0 {
		p.alias[info.SSRCRetransmission] = info.SSRC
	}
}

// SetTargetBitrate is GCC's estimate, which the pacer doesn't follow:
// it climbs while less is sent than it allows, and pacing at it would
// pace nothing. The room paces at what it gave the layers (setRate).
func (p *pacer) SetTargetBitrate(int) {}

// setRate paces video at bps (times paceFactor).
func (p *pacer) setRate(bps int) {
	p.mu.Lock()
	p.rate = paceFactor * float64(bps)
	p.mu.Unlock()
}

// Write sends an audio packet now and queues the rest.
func (p *pacer) Write(h *rtp.Header, payload []byte, attr interceptor.Attributes) (int, error) {
	p.mu.Lock()
	ssrc := h.SSRC
	if a, ok := p.alias[ssrc]; ok {
		ssrc = a
	}
	w := p.writers[ssrc]
	if w == nil {
		p.mu.Unlock()
		return 0, gcc.ErrUnknownStream
	}
	n := h.MarshalSize() + len(payload)
	if p.audio[ssrc] {
		p.sent.add(time.Now(), n)
		p.mu.Unlock()
		return w.Write(h, payload, attr)
	}
	p.q.PushBack(&paced{h: h.Clone(), p: append([]byte(nil), payload...), attr: attr, w: w, at: time.Now()})
	p.qBytes += n
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return n, nil
}

// run sends queued video as the budget allows: at once while there's
// budget, then every paceTick while packets wait.
func (p *pacer) run() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		held := p.drain()
		wait := time.Hour
		if held {
			wait = paceTick
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

// drain sends what the budget allows; it reports whether packets wait.
func (p *pacer) drain() bool {
	for {
		p.mu.Lock()
		now := time.Now()
		if !p.last.IsZero() {
			p.budget += now.Sub(p.last).Seconds() * p.rate / 8
		}
		p.last = now
		p.budget = min(p.budget, paceBurst.Seconds()*p.rate/8)
		e := p.q.Front()
		if e == nil || p.budget <= 0 {
			p.mu.Unlock()
			return e != nil
		}
		it := p.q.Remove(e).(*paced)
		n := it.h.MarshalSize() + len(it.p)
		p.qBytes -= n
		p.budget -= float64(n)
		p.sent.add(now, n)
		p.mu.Unlock()
		it.w.Write(&it.h, it.p, it.attr)
	}
}

// queued is how long the oldest packet held has waited, and how much is
// held.
func (p *pacer) queued() (time.Duration, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.q.Front(); e != nil {
		return time.Since(e.Value.(*paced).at), p.qBytes
	}
	return 0, 0
}

// sentRate is what went out over the last second, in bits a second.
func (p *pacer) sentRate() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent.rate(time.Now())
}

func (p *pacer) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

// rateMeter counts bytes over the last second in 100 ms bins.
type rateMeter struct {
	bins  [10]int
	start time.Time // the start of the newest bin
	i     int
}

func (m *rateMeter) roll(now time.Time) {
	if m.start.IsZero() {
		m.start = now
		return
	}
	for steps := 0; now.Sub(m.start) >= 100*time.Millisecond; steps++ {
		m.start = m.start.Add(100 * time.Millisecond)
		m.i = (m.i + 1) % len(m.bins)
		m.bins[m.i] = 0
		if steps > len(m.bins) {
			m.start = now
			clear(m.bins[:])
			break
		}
	}
}

func (m *rateMeter) add(now time.Time, n int) {
	m.roll(now)
	m.bins[m.i] += n
}

func (m *rateMeter) rate(now time.Time) int {
	m.roll(now)
	sum := 0
	for _, b := range m.bins {
		sum += b
	}
	return sum * 8
}

// estimator is one peer connection's bandwidth estimate: pion's GCC with
// our pacer.
type estimator struct {
	*gcc.SendSideBWE
	pacer *pacer
}

// AddStream tells the pacer what the stream is, then gives it to GCC.
func (e *estimator) AddStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	e.pacer.stream(info)
	return e.SendSideBWE.AddStream(info, w)
}

// GCC's range, and where it starts.
const (
	bweInitial = 1_200_000
	bweMin     = 100_000
	bweMax     = 8_000_000
)

// newCC is the congestion control interceptor; found gets each peer
// connection's estimator as it's made.
func newCC(found func(id string, e *estimator)) (*cc.InterceptorFactory, error) {
	f, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		p := newPacer(bweInitial)
		bwe, err := gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(bweInitial), gcc.SendSideBWEMinBitrate(bweMin),
			gcc.SendSideBWEMaxBitrate(bweMax), gcc.SendSideBWEPacer(p))
		if err != nil {
			p.Close()
			return nil, err
		}
		return &estimator{bwe, p}, nil
	})
	if err != nil {
		return nil, err
	}
	f.OnNewPeerConnection(func(id string, bwe cc.BandwidthEstimator) {
		if e, ok := bwe.(*estimator); ok {
			found(id, e)
		}
	})
	return f, nil
}
