package agent

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/opus"
)

// Voice out: the brain's 24 kHz PCM, resampled to 48 kHz, encoded as 20 ms
// Opus frames and written to the agent's track in real time.

const (
	outRate  = opus.SampleRate
	frameLen = opus.FrameSamples // 20 ms
	tailMs   = 300               // speaking lasts this long past the last frame
	// keepAlive is how many 20 ms ticks go by between answers before a
	// silent frame is sent, as a person's mic sends a DTX frame every
	// 400 ms: between answers the frames keep the receivers' jitter
	// windows going (a jitter buffer starts its target over after a second
	// with no packets) and show the track alive from the moment it's
	// published.
	keepAlive = 20
)

// Encoder turns 20 ms of 48 kHz mono PCM into one Opus packet.
type Encoder interface {
	Encode(pcm []int16) ([]byte, error)
	Close() error
}

// opusEncoder is the app's own Opus (internal/opus): 32 kbps with in-band
// FEC, like a person's mic, but without DTX: between answers run sends its
// own silent frame every 400 ms (keepAlive).
type opusEncoder struct {
	e   opus.Encoder
	f   []float32
	buf []byte
}

// NewEncoder is the default voice encoder.
func NewEncoder() (Encoder, error) {
	e, err := opus.NewEncoder(opus.Options{NoDTX: true})
	if err != nil {
		return nil, err
	}
	return &opusEncoder{e: e, f: make([]float32, frameLen), buf: make([]byte, opus.MaxPacket)}, nil
}

func (o *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	for i, s := range pcm[:frameLen] {
		o.f[i] = float32(s) / 32768
	}
	n, err := o.e.Encode(o.f, o.buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), o.buf[:n]...), nil
}

func (o *opusEncoder) Close() error { return o.e.Close() }

// voiceOut paces the voice onto the track.
type voiceOut struct {
	enc        Encoder
	onSpeaking func(bool)

	mu    sync.Mutex
	buf   []int16 // 48 kHz, waiting to go out
	last  int16   // the last 24 kHz sample, for interpolation
	on    bool
	lastF time.Time
}

func newVoiceOut(enc Encoder, onSpeaking func(bool)) *voiceOut {
	return &voiceOut{enc: enc, onSpeaking: onSpeaking}
}

// push queues brain PCM (s16 mono at rate).
func (v *voiceOut) push(pcm []byte, rate int) {
	n := len(pcm) / 2
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range n {
		s := int16(binary.LittleEndian.Uint16(pcm[2*i:]))
		switch rate {
		case outRate:
			v.buf = append(v.buf, s)
		case 24000:
			// Twice the rate: each sample, then halfway to the next.
			v.buf = append(v.buf, int16((int32(v.last)+int32(s))/2), s)
		default:
			// Nearest sample, for rates the brain doesn't use.
			for k := range outRate / rate {
				_ = k
				v.buf = append(v.buf, s)
			}
		}
		v.last = s
	}
}

// cut drops what hasn't gone out.
func (v *voiceOut) cut() {
	v.mu.Lock()
	v.buf = v.buf[:0]
	v.mu.Unlock()
}

// speaking reports whether the voice is being heard.
func (v *voiceOut) speaking() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.on
}

// run writes a frame every 20 ms while there is voice to send, and a silent
// one every 400 ms between answers (keepAlive). Each
// packet's RTP timestamp follows the clock, sent or not, as a person's mic
// does: a receiver sees the quiet between answers as time going by, where a
// timestamp that stood still would read as the network holding packets back
// and push its jitter buffer to the most it waits.
func (v *voiceOut) run(ctx context.Context, write func(packet []byte, ts uint32) error) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	frame, hush := make([]int16, frameLen), make([]int16, frameLen)
	start, ts0 := time.Now(), rand.Uint32()|1
	var last uint32
	var sent bool
	quiet := keepAlive // ticks since a frame went out
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			v.mu.Lock()
			var have bool
			if len(v.buf) > 0 {
				have = true
				n := copy(frame, v.buf)
				clear(frame[n:])
				v.buf = v.buf[n:]
				v.lastF = now
			}
			change := 0
			if have && !v.on {
				v.on, change = true, 1
			} else if !have && v.on && now.Sub(v.lastF) > tailMs*time.Millisecond {
				v.on, change = false, -1
			}
			v.mu.Unlock()
			out := frame
			if !have {
				quiet++
				out = hush
			}
			if have || quiet >= keepAlive {
				quiet = 0
				if pkt, err := v.enc.Encode(out); err == nil {
					// The tick's frame number (the tick's own time, rounded),
					// never the same as the last one sent.
					n := uint32((now.Sub(start) + 10*time.Millisecond) / (20 * time.Millisecond))
					if sent && n <= last {
						n = last + 1
					}
					last, sent = n, true
					write(pkt, ts0+n*frameLen)
				}
			}
			if change != 0 && v.onSpeaking != nil {
				v.onSpeaking(change > 0)
			}
		}
	}
}

func (v *voiceOut) close() { v.enc.Close() }
