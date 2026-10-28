// Package video is the app's video in Go memory: NV12 frames (a plane of
// brightness, then one of colour at half size with Cb and Cr interleaved,
// as cameras, ScreenCaptureKit and VideoToolbox hand them over), the
// latest-frame slot that hands frames to the window, a test pattern that
// carries its frame number, and H.264's byte formats. Pure.
package video

import (
	"image"
	"sync"
	"time"
)

// Frame is one NV12 picture. W and H are even.
type Frame struct {
	W, H              int
	Y, UV             []byte
	YStride, UVStride int
	At                time.Time // when it was captured (a remote's: when it was decoded)
	Mirror            bool      // shown flipped left to right (your own camera)
}

// NewFrame allocates a w×h frame with no row padding.
func NewFrame(w, h int) Frame {
	return Frame{W: w, H: h, Y: make([]byte, w*h), UV: make([]byte, w*h/2), YStride: w, UVStride: w}
}

// fits reports whether f's planes are big enough for w×h.
func (f *Frame) fits(w, h int) bool {
	return f.YStride >= w && f.UVStride >= w && len(f.Y) >= f.YStride*h && len(f.UV) >= f.UVStride*h/2
}

// CopyFrom makes f a copy of src, reusing f's planes when they're big
// enough.
func (f *Frame) CopyFrom(src Frame) {
	if !f.fits(src.W, src.H) {
		*f = NewFrame(src.W, src.H)
	}
	f.W, f.H, f.At, f.Mirror = src.W, src.H, src.At, src.Mirror
	for y := range src.H {
		copy(f.Y[y*f.YStride:y*f.YStride+src.W], src.Y[y*src.YStride:])
	}
	for y := range src.H / 2 {
		copy(f.UV[y*f.UVStride:y*f.UVStride+src.W], src.UV[y*src.UVStride:])
	}
}

// Latest is a one-frame slot: a producer puts each frame, the window takes
// the newest when it draws, and frames in between are dropped. Put copies
// into storage the slot keeps, so a producer can reuse its buffers.
type Latest struct {
	mu  sync.Mutex
	f   Frame
	seq uint64
}

// Put replaces the frame held.
func (l *Latest) Put(f Frame) {
	l.mu.Lock()
	l.f.CopyFrom(f)
	l.seq++
	l.mu.Unlock()
}

// Take calls fn with the frame held, under the slot's lock, if it's newer
// than after (a seq Take returned before; 0 for any), and returns its seq.
// fn must not keep the frame's planes.
func (l *Latest) Take(after uint64, fn func(Frame)) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seq == 0 || l.seq == after {
		return after, false
	}
	fn(l.f)
	return l.seq, true
}

// RGBA converts the frame to an image (BT.709, video range), for PNGs
// agents read.
func (f Frame) RGBA() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	clamp := func(v float64) uint8 { return uint8(min(max(v, 0), 255) + 0.5) }
	for y := range f.H {
		for x := range f.W {
			l := 1.164 * (float64(f.Y[y*f.YStride+x]) - 16)
			i := (y/2)*f.UVStride + x&^1
			cb, cr := float64(f.UV[i])-128, float64(f.UV[i+1])-128
			o := img.PixOffset(x, y)
			img.Pix[o] = clamp(l + 1.793*cr)
			img.Pix[o+1] = clamp(l - 0.213*cb - 0.533*cr)
			img.Pix[o+2] = clamp(l + 2.112*cb)
			img.Pix[o+3] = 255
		}
	}
	return img
}
