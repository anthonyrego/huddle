package agent

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	kopus "github.com/kazzmir/opus-go/opus"
)

// Voice out: the brain's 24 kHz PCM, resampled to 48 kHz, encoded as 20 ms
// Opus frames and written to the agent's track in real time.

const (
	outRate  = 48000
	frameLen = outRate / 50 // 20 ms
	tailMs   = 300          // speaking lasts this long past the last frame
)

// Encoder turns 20 ms of 48 kHz mono PCM into one Opus packet. It is the
// seam the app's own Opus package (internal/opus, from the voice track)
// replaces at integration.
type Encoder interface {
	Encode(pcm []int16) ([]byte, error)
	Close() error
}

// kazzmirEncoder is libopus transpiled to Go (no cgo). pion/opus has a
// pure-Go encoder too, but it is internal to that module.
type kazzmirEncoder struct {
	e   *kopus.Encoder
	buf []byte
}

// NewEncoder is the default voice encoder: 48 kHz mono, VoIP, 32 kbps.
func NewEncoder() (Encoder, error) {
	e, err := kopus.NewEncoder(outRate, 1, kopus.ApplicationVoIP)
	if err != nil {
		return nil, err
	}
	e.SetBitrate(32000)
	return &kazzmirEncoder{e: e, buf: make([]byte, 1500)}, nil
}

func (k *kazzmirEncoder) Encode(pcm []int16) ([]byte, error) {
	n, err := k.e.Encode(pcm, len(pcm), k.buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), k.buf[:n]...), nil
}

func (k *kazzmirEncoder) Close() error { return k.e.Close() }

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

// run writes a frame every 20 ms while there is voice to send.
func (v *voiceOut) run(ctx context.Context, write func(packet []byte, d time.Duration) error) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	frame := make([]int16, frameLen)
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
			if have {
				if pkt, err := v.enc.Encode(frame); err == nil {
					write(pkt, 20*time.Millisecond)
				}
			}
			if change != 0 && v.onSpeaking != nil {
				v.onSpeaking(change > 0)
			}
		}
	}
}

func (v *voiceOut) close() { v.enc.Close() }
