package audio

import (
	"sync"
	"time"
)

// paced holds mic audio that arrives in big chunks (AVAudioEngine's taps
// deliver 100 ms at a time, whatever is asked for) and gives it out evenly:
// the samples of a chunk that arrived at A are released from A on, one
// sample period apart, so 10 ms frames leave every 10 ms instead of ten at
// once. Sent in bursts, they would look to every receiver like 80 ms of
// network jitter. It costs nothing the chunking hasn't already: the newest
// sample of a chunk waits a chunk's length either way.
type paced struct {
	rate int
	max  int // the most samples held; the oldest go beyond it

	mu     sync.Mutex
	buf    []float32
	read   int64 // the absolute index of buf[0]
	chunks []chunkMark
	sig    chan struct{}
}

type chunkMark struct {
	start int64 // absolute index of the chunk's first sample
	at    time.Time
}

func newPaced(rate int, hold time.Duration) *paced {
	return &paced{rate: rate, max: int(int64(rate) * int64(hold) / int64(time.Second)), sig: make(chan struct{}, 1)}
}

// push adds a chunk that arrived at at.
func (p *paced) push(s []float32, at time.Time) {
	if len(s) == 0 {
		return
	}
	p.mu.Lock()
	p.chunks = append(p.chunks, chunkMark{start: p.read + int64(len(p.buf)), at: at})
	p.buf = append(p.buf, s...)
	if over := len(p.buf) - p.max; over > 0 {
		p.drop(over)
	}
	p.mu.Unlock()
	select {
	case p.sig <- struct{}{}:
	default:
	}
}

// drop forgets the n oldest samples. Call with mu held.
func (p *paced) drop(n int) {
	p.buf = p.buf[n:]
	p.read += int64(n)
	for len(p.chunks) > 1 && p.chunks[1].start <= p.read {
		p.chunks = p.chunks[1:]
	}
}

// take fills frame if its first sample is due by now. When it isn't, it
// says how long until it is (0: not enough audio yet; wait for a push).
func (p *paced) take(frame []float32, now time.Time) (ok bool, wait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) < len(frame) || len(p.chunks) == 0 {
		return false, 0
	}
	c := p.chunks[0]
	due := c.at.Add(time.Duration(p.read-c.start) * time.Second / time.Duration(p.rate))
	if d := due.Sub(now); d > 0 {
		return false, d
	}
	copy(frame, p.buf)
	p.drop(len(frame))
	if len(p.buf) == 0 {
		p.buf = p.buf[:0:0] // let the backing array go
	}
	return true, 0
}

// held is how many samples wait.
func (p *paced) held() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}
