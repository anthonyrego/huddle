// Package pcm holds helpers for mono 16-bit little-endian PCM, the format
// Live takes and sends: byte and time conversions, levels, and a speech
// detector used to time turns for the latency log.
//
// Copied from an earlier project of the author's.
package pcm

import (
	"encoding/binary"
	"math"
	"time"
)

// BytesPerSample is the size of one mono s16 sample.
const BytesPerSample = 2

// Silence is the level Level reports for an all-zero buffer.
const Silence = -120.0

// Duration is how long n bytes last at rate.
func Duration(n int64, rate int) time.Duration {
	return time.Duration(n) * time.Second / time.Duration(rate*BytesPerSample)
}

// Bytes is how many bytes d lasts at rate, rounded down to whole samples.
func Bytes(d time.Duration, rate int) int64 {
	n := int64(d) * int64(rate) / int64(time.Second)
	return n * BytesPerSample
}

// Level is the RMS level of b in dBFS: 0 is full scale, Silence is the floor.
// A trailing odd byte is ignored.
func Level(b []byte) float64 {
	n := len(b) / BytesPerSample
	if n == 0 {
		return Silence
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(b[i*2:])))
		sum += s * s
	}
	rms := math.Sqrt(sum/float64(n)) / 32768
	if rms == 0 {
		return Silence
	}
	return max(20*math.Log10(rms), Silence)
}

// Edge is a change in whether someone is speaking.
type Edge int

const (
	None Edge = iota
	Start
	End
)

// Detector turns chunk levels into speech start and end edges. A pause
// shorter than Hangover doesn't end speech, so the gaps between words don't
// split an utterance. It only times turns for the log; the server's own
// voice detection decides when the model answers.
type Detector struct {
	Threshold float64       // dBFS at or above which a chunk counts as speech
	Hangover  time.Duration // quiet this long ends speech
	// Attack is how long chunks must stay loud, with no quiet one between,
	// before speech starts: a click or a peak of noise isn't speech. Zero
	// starts on the first loud chunk.
	Attack time.Duration

	speaking  bool
	lastLoud  time.Time
	firstLoud time.Time // the start of the current run of loud chunks (not speaking)
}

// Speaking reports whether the detector is inside an utterance.
func (d *Detector) Speaking() bool { return d.speaking }

// Feed takes the level of the chunk that ended at now. It returns Start at
// now when speech begins (Attack after the first loud chunk of
// the run), and End with the time of the last loud chunk once
// Hangover of quiet has passed.
func (d *Detector) Feed(level float64, now time.Time) (Edge, time.Time) {
	if level >= d.Threshold {
		d.lastLoud = now
		if !d.speaking {
			if d.firstLoud.IsZero() {
				d.firstLoud = now
			}
			if now.Sub(d.firstLoud) < d.Attack {
				return None, time.Time{}
			}
			d.speaking, d.firstLoud = true, time.Time{}
			return Start, now
		}
		return None, time.Time{}
	}
	d.firstLoud = time.Time{}
	if d.speaking && now.Sub(d.lastLoud) >= d.Hangover {
		d.speaking = false
		return End, d.lastLoud
	}
	return None, time.Time{}
}
