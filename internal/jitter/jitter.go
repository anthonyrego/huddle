// Package jitter turns one remote voice track's packets, as they arrive
// (late, out of order, some never), into a steady stream of samples.
//
// A Buffer holds packets by sequence number and is pulled on the output
// clock. It waits for its target delay's worth of audio before it starts;
// a missing packet is recovered from the next one's in-band FEC when that
// one is already here, and concealed otherwise; a packet that comes after
// its turn is dropped. The target follows the interarrival jitter: the
// 95th percentile of how much later than the earliest packets arrived over
// the last few seconds, plus a frame, clamped to 40–300 ms. DTX silence
// (no packets while the sender is quiet) is played as silence, and the
// part of it beyond the target is skipped, so each talk spurt starts at the
// target delay. Pulling while nothing is here (an underrun) conceals
// without moving the stream on, which adds delay the playout removes later,
// at a silence.
package jitter

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/opus"
)

// Packet is the part of an RTP packet the buffer needs.
type Packet struct {
	Seq     uint16
	TS      uint32 // RTP timestamp at 48 kHz
	Payload []byte // one Opus packet
}

// Config tunes a Buffer. The zero value is the voice's defaults.
type Config struct {
	MinDelay, MaxDelay time.Duration // the target's range; 0 is 40 and 300 ms
	// Window is how many arrivals the target is computed over; 0 is 250
	// (5 s of 20 ms packets).
	Window int
	// Percentile of the arrival delays the target covers; 0 is 0.95.
	Percentile float64
}

func (c Config) withDefaults() Config {
	if c.MinDelay == 0 {
		c.MinDelay = 40 * time.Millisecond
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = 300 * time.Millisecond
	}
	if c.Window == 0 {
		c.Window = 250
	}
	if c.Percentile == 0 {
		c.Percentile = 0.95
	}
	return c
}

// Stats are a buffer's counts since it was made. Durations of made-up
// audio are in samples at 48 kHz.
type Stats struct {
	Received  int // packets pushed
	Late      int // arrived after their turn, dropped
	Duplicate int // arrived twice
	Lost      int // never here when needed
	FEC       int // lost packets recovered from the next one's FEC
	PLC       int // lost packets concealed
	Dropped   int // discarded because the buffer had grown far past its target
	Skipped   int // samples skipped to come back to the target (DTX silence, or a trim)
	Underrun  int // samples made up because nothing had arrived yet (not DTX)
	Errors    int // packets the decoder refused

	Target time.Duration // the delay aimed for now
	Level  time.Duration // the audio held now, ahead of what plays next
	Jitter time.Duration // RFC 3550 interarrival jitter
}

// Concealed is how many lost packets were made up, by FEC or concealment.
func (s Stats) Concealed() int { return s.FEC + s.PLC }

// Buffer is one remote track's jitter buffer. Push and Pull may be called
// from different goroutines.
type Buffer struct {
	mu  sync.Mutex
	cfg Config
	dec opus.Decoder

	pkts    map[int64]*entry // by extended sequence number
	seen    bool             // a packet has been pushed
	maxSeq  int64            // highest extended sequence number pushed
	maxTS   int64            // highest extended timestamp pushed
	maxEnd  int64            // the end (timestamp + length) of the newest audio pushed
	started bool
	next    int64     // the sequence number to play next
	playTS  int64     // the timestamp of the next sample to play
	pending []float32 // decoded samples from playTS on, not yet pulled
	buf     []float32 // the decoder's output
	inDTX   bool      // the last packet played was DTX: the sender is quiet
	plcRun  int       // samples concealed in a row during an underrun

	baseAt      time.Time // arrival of the first packet, for transit times
	baseTS      int64
	transits    []float64 // ring of recent relative transit times, seconds
	ti          int
	lastTransit float64
	jitter      float64 // RFC 3550, seconds
	target      time.Duration

	st Stats
}

type entry struct {
	seq, ts int64
	n       int // samples
	payload []byte
}

// New returns a buffer that decodes with dec, which it owns.
func New(dec opus.Decoder, cfg Config) *Buffer {
	cfg = cfg.withDefaults()
	return &Buffer{
		cfg:    cfg,
		dec:    dec,
		pkts:   map[int64]*entry{},
		buf:    make([]float32, 6*opus.FrameSamples),
		target: cfg.MinDelay,
	}
}

// Close releases the decoder.
func (b *Buffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dec.Close()
}

const rate = opus.SampleRate

func samples(d time.Duration) int64 { return int64(d) * rate / int64(time.Second) }

func dur(n int64) time.Duration { return time.Duration(n) * time.Second / rate }

// Push adds a packet that arrived at at.
func (b *Buffer) Push(p Packet, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Received++
	var seq, ts int64
	if !b.seen {
		seq, ts = int64(p.Seq), int64(p.TS)
		b.seen, b.maxSeq, b.maxTS = true, seq, ts
	} else {
		seq = b.maxSeq + int64(int16(p.Seq-uint16(b.maxSeq)))
		ts = b.maxTS + int64(int32(p.TS-uint32(b.maxTS)))
	}
	b.observe(ts, at)
	if b.started && seq < b.next {
		b.st.Late++
		return
	}
	if _, dup := b.pkts[seq]; dup {
		b.st.Duplicate++
		return
	}
	if b.started && (seq-b.next > 1000 || ts-b.playTS > 10*rate) {
		b.restart() // the sender started over
	}
	n := opus.PacketSamples(p.Payload)
	if n <= 0 {
		n = opus.FrameSamples
	}
	b.pkts[seq] = &entry{seq: seq, ts: ts, n: n, payload: append([]byte(nil), p.Payload...)}
	b.maxSeq = max(b.maxSeq, seq)
	b.maxTS = max(b.maxTS, ts)
	b.maxEnd = max(b.maxEnd, ts+int64(n))
}

// restart forgets the stream so the next packets start it again.
func (b *Buffer) restart() {
	clear(b.pkts)
	b.started, b.pending, b.inDTX = false, nil, false
	b.maxEnd = 0
}

// observe updates the jitter estimate and the target with one arrival.
func (b *Buffer) observe(ts int64, at time.Time) {
	if b.baseAt.IsZero() {
		b.baseAt, b.baseTS = at, ts
	}
	transit := at.Sub(b.baseAt).Seconds() - float64(ts-b.baseTS)/rate
	if len(b.transits) > 0 {
		b.jitter += (math.Abs(transit-b.lastTransit) - b.jitter) / 16
	}
	b.lastTransit = transit
	if len(b.transits) < b.cfg.Window {
		b.transits = append(b.transits, transit)
	} else {
		b.transits[b.ti] = transit
		b.ti = (b.ti + 1) % b.cfg.Window
	}
	// How much later than the quickest each packet was, at the percentile.
	sorted := slices.Clone(b.transits)
	slices.Sort(sorted)
	p := sorted[min(int(b.cfg.Percentile*float64(len(sorted))), len(sorted)-1)] - sorted[0]
	t := time.Duration(p*float64(time.Second)) + dur(opus.FrameSamples)
	b.target = min(max(t, b.cfg.MinDelay), b.cfg.MaxDelay)
}

// level is how much audio is held ahead of the next sample, in samples.
func (b *Buffer) level() int64 {
	if !b.started {
		if e := b.earliest(); e != nil {
			return b.maxEnd - e.ts
		}
		return 0
	}
	return max(b.maxEnd-b.playTS, 0)
}

// earliest is the held packet with the lowest sequence number.
func (b *Buffer) earliest() *entry {
	var first *entry
	for _, e := range b.pkts {
		if first == nil || e.seq < first.seq {
			first = e
		}
	}
	return first
}

// heldAfter is the held packet with the lowest sequence number above seq.
func (b *Buffer) heldAfter(seq int64) *entry {
	var first *entry
	for _, e := range b.pkts {
		if e.seq > seq && (first == nil || e.seq < first.seq) {
			first = e
		}
	}
	return first
}

// Pull fills out with the next samples.
func (b *Buffer) Pull(out []float32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for filled := 0; filled < len(out); {
		if len(b.pending) > 0 {
			k := copy(out[filled:], b.pending)
			b.pending = b.pending[k:]
			b.playTS += int64(k)
			filled += k
			continue
		}
		if !b.started && !b.start() {
			clear(out[filled:])
			return
		}
		if !b.produce() {
			b.underrun(out[filled:])
			return
		}
	}
}

// start begins playing once the target's worth of audio is held.
func (b *Buffer) start() bool {
	e := b.earliest()
	if e == nil || b.maxEnd-e.ts < samples(b.target) {
		return false
	}
	b.started, b.next, b.playTS = true, e.seq, e.ts
	return true
}

// produce puts the next stretch of the stream into pending. It returns false
// when there is nothing to play yet.
func (b *Buffer) produce() bool {
	b.trim()
	e := b.pkts[b.next]
	if e == nil {
		if b.maxSeq <= b.next {
			return false // nothing later either: an underrun (or DTX)
		}
		b.conceal()
		return true
	}
	if e.ts > b.playTS {
		// A gap in time with no packet missing: the sender was quiet (DTX).
		// Play silence, skipping what is beyond the target.
		if over := b.level() - samples(b.target); over > 0 {
			skip := min(over, e.ts-b.playTS)
			b.playTS += skip
			b.st.Skipped += int(skip)
		}
		if gap := e.ts - b.playTS; gap > 0 {
			b.silence(int(min(gap, opus.FrameSamples)))
			return true
		}
	}
	b.playTS = e.ts
	delete(b.pkts, e.seq)
	b.next = e.seq + 1
	if opus.IsDTX(len(e.payload)) {
		b.silence(e.n)
		b.inDTX = true
		return true
	}
	n, err := b.dec.Decode(e.payload, b.buf)
	if err != nil || n == 0 {
		b.st.Errors++
		n, _ = b.dec.Conceal(b.buf[:e.n])
	}
	b.pending = b.buf[:n]
	b.inDTX, b.plcRun = false, 0
	return true
}

// conceal makes up the packet at next, which never came though later ones
// did: from the next packet's FEC when that one is here, else concealed.
func (b *Buffer) conceal() {
	b.st.Lost++
	n := int64(opus.FrameSamples)
	h := b.heldAfter(b.next)
	if h != nil && h.ts > b.playTS {
		// Share the time up to the next packet held among the missing ones,
		// in whole 2.5 ms steps.
		n = (h.ts - b.playTS) / (h.seq - b.next)
		n = min(max(n/120*120, 120), 6*opus.FrameSamples)
	}
	out := b.buf[:n]
	switch nx := b.pkts[b.next+1]; {
	case b.inDTX:
		clear(out) // lost in a silence
	case nx != nil && b.dec.HasFEC() && !opus.IsDTX(len(nx.payload)):
		if k, err := b.dec.DecodeFEC(nx.payload, out); err == nil {
			out = out[:k]
			b.st.FEC++
			break
		}
		fallthrough
	default:
		if k, err := b.dec.Conceal(out); err == nil {
			out = out[:k]
		}
		b.st.PLC++
	}
	b.pending = out
	b.next++
}

// silence puts n samples of silence into pending.
func (b *Buffer) silence(n int) {
	out := b.buf[:min(n, len(b.buf))]
	clear(out)
	b.pending = out
}

// underrun fills out while nothing is here: concealment for up to 100 ms,
// then silence; DTX is silence without counting. The stream doesn't move
// on, so this adds delay.
func (b *Buffer) underrun(out []float32) {
	if b.inDTX || b.plcRun >= rate/10 {
		clear(out)
	} else {
		n := len(out) / 120 * 120 // concealment comes in 2.5 ms steps
		if k, err := b.dec.Conceal(out[:n]); err != nil || k != n {
			clear(out[:n])
		}
		clear(out[n:])
		b.plcRun += len(out)
	}
	if !b.inDTX {
		b.st.Underrun += len(out)
	}
}

// trim drops audio when the buffer has grown far past the target (a burst
// after a long stall), back to the target: a jump the listener hears, so
// only past MaxDelay plus 100 ms.
func (b *Buffer) trim() {
	if b.level() <= samples(b.cfg.MaxDelay+100*time.Millisecond) {
		return
	}
	to := b.maxEnd - samples(b.target)
	for b.next <= b.maxSeq {
		e := b.pkts[b.next]
		if e != nil && e.ts+int64(e.n) > to {
			break
		}
		if e != nil {
			delete(b.pkts, e.seq)
			b.st.Dropped++
		}
		b.next++
	}
	from := b.playTS
	b.playTS = max(b.playTS, min(to, b.maxEnd))
	if e := b.pkts[b.next]; e != nil {
		b.playTS = max(min(b.playTS, e.ts), from)
	}
	b.st.Skipped += int(b.playTS - from)
	b.pending = nil
}

// Level is how much audio is held ahead of what plays next.
func (b *Buffer) Level() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return dur(b.level())
}

// Target is the delay the buffer aims for now.
func (b *Buffer) Target() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target
}

// Stats returns the counts so far.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.st
	s.Target, s.Level = b.target, dur(b.level())
	s.Jitter = time.Duration(b.jitter * float64(time.Second))
	return s
}
