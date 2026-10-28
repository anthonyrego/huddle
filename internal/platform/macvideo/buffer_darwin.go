package macvideo

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/anthonyrego/huddle/internal/video"
)

// Buffer is a retained CVPixelBuffer: a picture in memory the frameworks
// share (an IOSurface), passed between capture, the encoder and the
// decoder without copying. Its owner releases it.
type Buffer struct{ ref uintptr }

// lockReadOnly is kCVPixelBufferLock_ReadOnly.
const lockReadOnly = 1

// NewBuffer makes an NV12 buffer backed by an IOSurface.
func NewBuffer(w, h int) (*Buffer, error) {
	if err := load(); err != nil {
		return nil, err
	}
	attrs := cfDict(k("kCVPixelBufferIOSurfacePropertiesKey"), cfDict())
	defer cf.release(attrs)
	var ref uintptr
	if err := check("CVPixelBufferCreate", cv.create(0, w, h, formatNV12, attrs, &ref)); err != nil {
		return nil, err
	}
	return &Buffer{ref}, nil
}

// retainBuffer takes a reference of its own to a CVPixelBuffer.
func retainBuffer(ref uintptr) *Buffer { return &Buffer{cf.retain(ref)} }

// Release lets the buffer go.
func (b *Buffer) Release() {
	if b != nil && b.ref != 0 {
		cf.release(b.ref)
		b.ref = 0
	}
}

// Size is the buffer's size in pixels.
func (b *Buffer) Size() (w, h int) { return cv.width(b.ref), cv.height(b.ref) }

// planes locks the buffer and returns its NV12 planes; unlock with the
// returned func.
func (b *Buffer) planes(flags uint64) (y, uv []byte, ys, uvs int, unlock func(), err error) {
	if f := cv.format(b.ref); f != formatNV12 && f != fourCC("420f") {
		return nil, nil, 0, 0, nil, fmt.Errorf("pixel buffer is %08x, not NV12", f)
	}
	if cv.planeCount(b.ref) != 2 {
		return nil, nil, 0, 0, nil, errors.New("pixel buffer isn't two planes")
	}
	if err := check("CVPixelBufferLockBaseAddress", cv.lock(b.ref, flags)); err != nil {
		return nil, nil, 0, 0, nil, err
	}
	_, h := b.Size()
	ys, uvs = cv.planeStride(b.ref, 0), cv.planeStride(b.ref, 1)
	y = unsafe.Slice((*byte)(cv.planeBase(b.ref, 0)), ys*h)
	uv = unsafe.Slice((*byte)(cv.planeBase(b.ref, 1)), uvs*h/2)
	return y, uv, ys, uvs, func() { cv.unlock(b.ref, flags) }, nil
}

// CopyTo copies the picture into f (reusing f's planes when they fit).
func (b *Buffer) CopyTo(f *video.Frame) error {
	y, uv, ys, uvs, unlock, err := b.planes(lockReadOnly)
	if err != nil {
		return err
	}
	defer unlock()
	w, h := b.Size()
	f.CopyFrom(video.Frame{W: w, H: h, Y: y, UV: uv, YStride: ys, UVStride: uvs})
	return nil
}

// CopyFrom fills the buffer from f, which must be its size.
func (b *Buffer) CopyFrom(f video.Frame) error {
	w, h := b.Size()
	if f.W != w || f.H != h {
		return fmt.Errorf("a %dx%d frame into a %dx%d buffer", f.W, f.H, w, h)
	}
	y, uv, ys, uvs, unlock, err := b.planes(0)
	if err != nil {
		return err
	}
	defer unlock()
	for r := range h {
		copy(y[r*ys:r*ys+w], f.Y[r*f.YStride:])
	}
	for r := range h / 2 {
		copy(uv[r*uvs:r*uvs+w], f.UV[r*f.UVStride:])
	}
	return nil
}

// Scaler resizes pictures (VTPixelTransferSession).
type Scaler struct{ s uintptr }

// NewScaler makes a scaler.
func NewScaler() (*Scaler, error) {
	if err := load(); err != nil {
		return nil, err
	}
	var s uintptr
	if err := check("VTPixelTransferSessionCreate", vt.xferCreate(0, &s)); err != nil {
		return nil, err
	}
	return &Scaler{s}, nil
}

// Scale draws src into dst at dst's size.
func (s *Scaler) Scale(src, dst *Buffer) error {
	return check("VTPixelTransferSessionTransferImage", vt.xferImage(s.s, src.ref, dst.ref))
}

// Close ends the session.
func (s *Scaler) Close() {
	if s.s != 0 {
		vt.xferInvalidate(s.s)
		cf.release(s.s)
		s.s = 0
	}
}

// Edit calls fn with the buffer's planes as a frame to write into, locked
// for writing; fn must not keep them.
func (b *Buffer) Edit(fn func(f video.Frame)) error {
	y, uv, ys, uvs, unlock, err := b.planes(0)
	if err != nil {
		return err
	}
	defer unlock()
	w, h := b.Size()
	fn(video.Frame{W: w, H: h, Y: y, UV: uv, YStride: ys, UVStride: uvs})
	return nil
}

// View calls fn with the buffer's planes as a frame, locked for reading;
// fn must not keep them.
func (b *Buffer) View(fn func(f video.Frame)) error {
	y, uv, ys, uvs, unlock, err := b.planes(lockReadOnly)
	if err != nil {
		return err
	}
	defer unlock()
	w, h := b.Size()
	fn(video.Frame{W: w, H: h, Y: y, UV: uv, YStride: ys, UVStride: uvs})
	return nil
}

// Retain takes another reference: the copy is released on its own.
func (b *Buffer) Retain() *Buffer { return retainBuffer(b.ref) }

// Pool hands out NV12 buffers of one size, recycled once released.
type Pool struct {
	ref  uintptr
	w, h int
}

// NewPool makes a pool of w×h NV12 buffers backed by IOSurfaces.
func NewPool(w, h int) (*Pool, error) {
	if err := load(); err != nil {
		return nil, err
	}
	attrs := cfDict(
		k("kCVPixelBufferPixelFormatTypeKey"), cfInt(int64(formatNV12)),
		k("kCVPixelBufferWidthKey"), cfInt(int64(w)),
		k("kCVPixelBufferHeightKey"), cfInt(int64(h)),
		k("kCVPixelBufferIOSurfacePropertiesKey"), cfDict())
	defer cf.release(attrs)
	p := &Pool{w: w, h: h}
	if err := check("CVPixelBufferPoolCreate", cv.poolCreate(0, 0, attrs, &p.ref)); err != nil {
		return nil, err
	}
	return p, nil
}

// Get is a buffer from the pool.
func (p *Pool) Get() (*Buffer, error) {
	var ref uintptr
	if err := check("CVPixelBufferPoolCreatePixelBuffer", cv.poolCreateBuf(0, p.ref, &ref)); err != nil {
		return nil, err
	}
	return &Buffer{ref}, nil
}

// Close lets the pool go (buffers out of it stay valid until released).
func (p *Pool) Close() {
	if p.ref != 0 {
		cf.release(p.ref)
		p.ref = 0
	}
}
