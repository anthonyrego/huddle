package macvideo

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/anthonyrego/huddle/internal/video"
)

// Decoder is a VTDecompressionSession turning H.264 access units back into
// pictures. A stream that changes size (a simulcast layer switch) brings
// new parameter sets; the session takes them if it can, and is made again
// if it can't.
type Decoder struct {
	s, fd    uintptr
	id       uintptr
	rec      *decodeRecord // kept alive with the session
	sps, pps []byte

	// Rebuilds counts sessions made again for a new format.
	Rebuilds int

	mu     sync.Mutex
	target *video.Frame
	got    bool
	err    error
}

// decodeRecord is VTDecompressionOutputCallbackRecord.
type decodeRecord struct{ cb, refcon uintptr }

var decoders = struct {
	sync.Mutex
	m    map[uintptr]*Decoder
	next uintptr
}{m: map[uintptr]*Decoder{}}

var decodeCB = sync.OnceValue(func() uintptr { return purego.NewCallback(decodeOutput) })

// NewDecoder makes a decoder; its session starts with the first
// parameter sets.
func NewDecoder() (*Decoder, error) {
	if err := load(); err != nil {
		return nil, err
	}
	d := &Decoder{}
	decoders.Lock()
	decoders.next++
	d.id = decoders.next
	decoders.m[d.id] = d
	decoders.Unlock()
	d.rec = &decodeRecord{decodeCB(), d.id}
	return d, nil
}

// Decode decodes one Annex-B access unit into f, reusing f's planes when
// they fit. It reports false when the unit made no picture: parameter sets
// alone, or slices before the first parameter sets.
func (d *Decoder) Decode(au []byte, f *video.Frame) (bool, error) {
	var sps, pps []byte
	var slices [][]byte
	for _, n := range video.SplitAnnexB(au) {
		switch video.NALType(n) {
		case video.NALSPS:
			sps = n
		case video.NALPPS:
			pps = n
		case video.NALSlice, video.NALIDR:
			slices = append(slices, n)
		}
	}
	if sps != nil && pps != nil && (!bytes.Equal(sps, d.sps) || !bytes.Equal(pps, d.pps)) {
		if err := d.setFormat(sps, pps); err != nil {
			return false, err
		}
	}
	if d.s == 0 || len(slices) == 0 {
		return false, nil
	}
	data := video.AppendAVCC(nil, slices...)
	var bb, sb uintptr
	const assureMemoryNow = 1 // kCMBlockBufferAssureMemoryNowFlag
	if err := check("CMBlockBufferCreateWithMemoryBlock", cm.blockCreate(0, 0, len(data), 0, 0, 0, len(data), assureMemoryNow, &bb)); err != nil {
		return false, err
	}
	defer cf.release(bb)
	if err := check("CMBlockBufferReplaceDataBytes", cm.blockReplace(unsafe.Pointer(&data[0]), bb, 0, len(data))); err != nil {
		return false, err
	}
	size := uintptr(len(data))
	if err := check("CMSampleBufferCreateReady", cm.sampleCreate(0, bb, d.fd, 1, 0, 0, 1, &size, &sb)); err != nil {
		return false, err
	}
	defer cf.release(sb)
	d.mu.Lock()
	d.target, d.got, d.err = f, false, nil
	d.mu.Unlock()
	// Without kVTDecodeFrame_EnableAsynchronousDecompression the output
	// callback has run by the time this returns.
	var info uint32
	if err := check("VTDecompressionSessionDecodeFrame", vt.decFrame(d.s, sb, 0, 0, &info)); err != nil {
		return false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.target = nil
	return d.got, d.err
}

// setFormat takes new parameter sets.
func (d *Decoder) setFormat(sps, pps []byte) error {
	ptrs := []unsafe.Pointer{unsafe.Pointer(&sps[0]), unsafe.Pointer(&pps[0])}
	sizes := []uintptr{uintptr(len(sps)), uintptr(len(pps))}
	var fd uintptr
	err := check("CMVideoFormatDescriptionCreateFromH264ParameterSets", cm.fdFromParams(0, 2, &ptrs[0], &sizes[0], 4, &fd))
	runtime.KeepAlive(sps)
	runtime.KeepAlive(pps)
	if err != nil {
		return err
	}
	d.sps, d.pps = bytes.Clone(sps), bytes.Clone(pps)
	if d.s != 0 && vt.decCanAccept(d.s, fd) {
		cf.release(d.fd)
		d.fd = fd
		return nil
	}
	if d.s != 0 {
		vt.decInvalidate(d.s)
		cf.release(d.s)
		d.s = 0
		d.Rebuilds++
	}
	if d.fd != 0 {
		cf.release(d.fd)
	}
	d.fd = fd
	attrs := cfDict(
		k("kCVPixelBufferPixelFormatTypeKey"), cfInt(int64(formatNV12)),
		k("kCVPixelBufferIOSurfacePropertiesKey"), cfDict())
	defer cf.release(attrs)
	if err := check("VTDecompressionSessionCreate", vt.decCreate(0, fd, 0, attrs, d.rec, &d.s)); err != nil {
		d.s = 0
		return err
	}
	v := cfBool(true)
	vt.setProperty(d.s, k("kVTDecompressionPropertyKey_RealTime"), v)
	cf.release(v)
	return nil
}

// Size is the size the stream's parameter sets give (0, 0 before any).
func (d *Decoder) Size() (w, h int) {
	if d.fd == 0 {
		return 0, 0
	}
	v := cm.fdDimensions(d.fd)
	return int(int32(v)), int(int32(v >> 32))
}

// Close ends the session.
func (d *Decoder) Close() {
	if d.s != 0 {
		vt.decInvalidate(d.s)
		cf.release(d.s)
		d.s = 0
	}
	if d.fd != 0 {
		cf.release(d.fd)
		d.fd = 0
	}
	decoders.Lock()
	delete(decoders.m, d.id)
	decoders.Unlock()
}

// decodeOutput is VideoToolbox's output callback. The two CMTimes after
// the image are 24-byte structs, which arm64 passes as pointers to copies
// (and purego callbacks take no structs), so they arrive as uintptrs.
func decodeOutput(refcon, frameRef uintptr, status int32, flags uint32, image, pts, dur uintptr) {
	decoders.Lock()
	d := decoders.m[refcon]
	decoders.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.target == nil {
		return
	}
	if status != 0 || image == 0 {
		d.err = fmt.Errorf("decode: OSStatus %d", status)
		return
	}
	b := Buffer{image} // not ours: valid for the callback
	if d.err = b.CopyTo(d.target); d.err == nil {
		d.got = true
	}
}
