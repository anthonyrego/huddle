// Package opus encodes and decodes the voice: 48 kHz mono, 20 ms frames,
// in-band FEC and DTX, in pure Go. It puts one interface over two
// libraries so they can be measured against each other:
//
//   - Libopus: github.com/kazzmir/opus-go, libopus 1.6 transpiled to Go.
//     It encodes and decodes, with FEC and the codec's own concealment.
//   - Pion: github.com/pion/opus, a decoder written in Go. It has no
//     public encoder, no FEC and no concealment; Conceal repeats the last
//     frame, fading.
//
// NewEncoder and NewDecoder return the one the measurements picked
// (Libopus; docs/design.md, "Voice", says why).
package opus

import (
	"errors"
	"fmt"
)

// The format the voice runs in.
const (
	SampleRate   = 48000
	FrameSamples = SampleRate / 50 // 20 ms
	// MaxPacket is the most an encoded 20 ms frame takes here.
	MaxPacket = 1500
)

// IsDTX reports whether an encoded packet is a DTX frame (the encoder
// heard silence and needs nothing sent): libopus's "two bytes or less".
func IsDTX(n int) bool { return n <= 2 }

// Options configure an encoder. The zero value is the voice's defaults.
type Options struct {
	Bitrate     int  // bits per second; 0 is 32000
	Complexity  int  // 0–10; 0 is 9
	LossPercent int  // the loss FEC is sized for; 0 is 10
	NoFEC       bool // turn in-band FEC off
	NoDTX       bool // turn DTX off
}

// Encoder turns 20 ms frames into packets.
type Encoder interface {
	// Encode encodes one frame of FrameSamples into out and returns the
	// packet's length. A length IsDTX reports doesn't need sending.
	Encode(pcm []float32, out []byte) (int, error)
	Close() error
}

// Decoder turns packets into frames, and makes up frames that never came.
type Decoder interface {
	// Decode decodes one packet into out (at least FrameSamples) and
	// returns how many samples it wrote.
	Decode(packet []byte, out []float32) (int, error)
	// DecodeFEC recovers the frame before next from next's in-band FEC,
	// len(out) samples (the lost frame's length, a multiple of 2.5 ms).
	// Without FEC in next it conceals instead.
	DecodeFEC(next []byte, out []float32) (int, error)
	// Conceal makes up len(out) samples that were lost.
	Conceal(out []float32) (int, error)
	// HasFEC reports whether DecodeFEC can recover anything at all.
	HasFEC() bool
	Close() error
}

// Backend names a library.
type Backend string

const (
	Libopus Backend = "libopus" // kazzmir/opus-go
	Pion    Backend = "pion"    // pion/opus
)

// Chosen is the backend NewEncoder and NewDecoder use.
const Chosen = Libopus

// NewEncoder returns an encoder from the chosen backend.
func NewEncoder(o Options) (Encoder, error) { return NewEncoderWith(Chosen, o) }

// NewDecoder returns a decoder from the chosen backend.
func NewDecoder() (Decoder, error) { return NewDecoderWith(Chosen) }

// ErrNoEncoder is what NewEncoderWith says for Pion.
var ErrNoEncoder = errors.New("opus: pion/opus has no public encoder")

// NewEncoderWith returns an encoder from backend b.
func NewEncoderWith(b Backend, o Options) (Encoder, error) {
	switch b {
	case Libopus:
		return newLibopusEncoder(o)
	case Pion:
		return nil, ErrNoEncoder
	}
	return nil, fmt.Errorf("opus: no backend %q", b)
}

// NewDecoderWith returns a decoder from backend b.
func NewDecoderWith(b Backend) (Decoder, error) {
	switch b {
	case Libopus:
		return newLibopusDecoder()
	case Pion:
		return newPionDecoder()
	}
	return nil, fmt.Errorf("opus: no backend %q", b)
}

// PacketSamples is how many samples (at 48 kHz) a packet holds, read from
// its table-of-contents byte (RFC 6716 §3.1), or 0 when it isn't an Opus
// packet.
func PacketSamples(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	toc := p[0]
	cfg := int(toc >> 3)
	var frame int // in units of 2.5 ms
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		frame = []int{4, 8, 16, 24}[cfg%4]
	case cfg < 16: // hybrid: 10, 20 ms
		frame = []int{4, 8}[cfg%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = []int{1, 2, 4, 8}[cfg%4]
	}
	frames := 1
	switch toc & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0
		}
		frames = int(p[1] & 0x3f)
	}
	return frames * frame * SampleRate / 400
}
