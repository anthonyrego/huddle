package opus

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	libc "github.com/kazzmir/opus-go/libcshim"
	"github.com/kazzmir/opus-go/opuscc"
	"github.com/kazzmir/opus-go/opusccenc"
)

// Both sides talk to the transpiled libopus directly rather than through
// opus-go's Encoder and Decoder: the Encoder takes s16 only and hides the
// CTLs for FEC, DTX and the expected loss, and both create their shim
// "TLS" (the C stack the transpiled code allocates its locals on) at its
// default 64 KB. When a call needs more, the shim grows that stack by
// copying it to a new slice, which leaves the addresses the callers up the
// chain hold pointing into the old one; once the garbage collector frees it,
// libopus reads garbage (seen as a SILK resampler assertion during
// concealment, only with the GC running). newTLS grows the stack once, up
// front, far past what a mono 48 kHz codec uses, so it never moves.

// tlsStack is the C stack each codec gets.
const tlsStack = 1 << 20

func newTLS() *libc.TLS {
	tls := libc.NewTLS()
	tls.Alloc(tlsStack)
	tls.Free(tlsStack)
	return tls
}

type libopusEncoder struct {
	mu      sync.Mutex
	tls     *libc.TLS
	st      uintptr
	in, out cBuf
}

func newLibopusEncoder(o Options) (*libopusEncoder, error) {
	tls := newTLS()
	var st uintptr
	var err error
	onPinned(func() {
		st, err = opusccenc.Opus_opus_encoder_create(tls, SampleRate, 1, opusccenc.OPUS_APPLICATION_VOIP)
	})
	if err != nil || st == 0 {
		opusccenc.FreePseudostackTLS(tls)
		tls.Close()
		return nil, fmt.Errorf("opus: create encoder: %v", err)
	}
	e := &libopusEncoder{tls: tls, st: st}
	bitrate, complexity, loss := o.Bitrate, o.Complexity, o.LossPercent
	if bitrate == 0 {
		bitrate = 32000
	}
	if complexity == 0 {
		complexity = 9
	}
	if loss == 0 {
		loss = 10
	}
	fec, dtx := int32(1), int32(1)
	if o.NoFEC {
		fec = 0
	}
	if o.NoDTX {
		dtx = 0
	}
	for _, c := range []struct {
		req int32
		v   int32
	}{
		{opusccenc.OPUS_SET_BITRATE_REQUEST, int32(bitrate)},
		{opusccenc.OPUS_SET_COMPLEXITY_REQUEST, int32(complexity)},
		{opusccenc.OPUS_SET_SIGNAL_REQUEST, opusccenc.OPUS_SIGNAL_VOICE},
		{opusccenc.OPUS_SET_INBAND_FEC_REQUEST, fec},
		{opusccenc.OPUS_SET_PACKET_LOSS_PERC_REQUEST, int32(loss)},
		{opusccenc.OPUS_SET_DTX_REQUEST, dtx},
	} {
		if err := e.ctl(c.req, c.v); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

func (e *libopusEncoder) ctl(req, v int32) error {
	bp := e.tls.Alloc(16)
	defer e.tls.Free(16)
	var ret int32
	onPinned(func() { ret = opusccenc.Opus_opus_encoder_ctl(e.tls, e.st, req, libc.VaList(bp, v)) })
	if ret != opusccenc.OPUS_OK {
		return fmt.Errorf("opus: encoder ctl %d = %d: error %d", req, v, ret)
	}
	return nil
}

func (e *libopusEncoder) Encode(pcm []float32, out []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st == 0 {
		return 0, errors.New("opus: encoder closed")
	}
	if len(pcm) < FrameSamples {
		return 0, fmt.Errorf("opus: a frame is %d samples, got %d", FrameSamples, len(pcm))
	}
	in := copyIn(e.tls, &e.in, pcm[:FrameSamples])
	dst := e.out.ensure(e.tls, len(out))
	var n opusccenc.OpusT_opus_int32
	onPinned(func() {
		n = opusccenc.Opus_opus_encode_float(e.tls, e.st, in, FrameSamples, dst, opusccenc.OpusT_opus_int32(len(out)))
	})
	if n < 0 {
		return 0, fmt.Errorf("opus: encode: error %d", n)
	}
	copy(out, cSlice[byte](dst, int(n)))
	return int(n), nil
}

func (e *libopusEncoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st != 0 {
		onPinned(func() { opusccenc.Opus_opus_encoder_destroy(e.tls, e.st) })
		e.st = 0
		e.in.free(e.tls)
		e.out.free(e.tls)
		opusccenc.FreePseudostackTLS(e.tls)
		e.tls.Close()
	}
	return nil
}

// libopusDecoder is libopus's decoder, with its own FEC and concealment.
type libopusDecoder struct {
	mu      sync.Mutex
	tls     *libc.TLS
	st      uintptr
	in, out cBuf
}

func newLibopusDecoder() (*libopusDecoder, error) {
	tls := newTLS()
	var st uintptr
	var err error
	onPinned(func() { st, err = opuscc.Opus_opus_decoder_create(tls, SampleRate, 1) })
	if err != nil || st == 0 {
		opuscc.FreePseudostackTLS(tls)
		tls.Close()
		return nil, fmt.Errorf("opus: create decoder: %v", err)
	}
	return &libopusDecoder{tls: tls, st: st}, nil
}

// decode is opus_decode_float: packet nil conceals frame samples.
func (d *libopusDecoder) decode(packet []byte, out []float32, frame int, fec bool) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st == 0 {
		return 0, errors.New("opus: decoder closed")
	}
	if len(out) < frame {
		return 0, fmt.Errorf("opus: room for %d samples, need %d", len(out), frame)
	}
	var in uintptr
	if len(packet) > 0 {
		in = copyIn(d.tls, &d.in, packet)
	}
	dst := d.out.ensure(d.tls, frame*4)
	f := int32(0)
	if fec {
		f = 1
	}
	var n int32
	onPinned(func() {
		n = opuscc.Opus_opus_decode_float(d.tls, d.st, in, opuscc.OpusT_opus_int32(len(packet)), dst, int32(frame), f)
	})
	if n < 0 {
		return 0, fmt.Errorf("opus: decode: error %d", n)
	}
	copy(out, cSlice[float32](dst, int(n)))
	return int(n), nil
}

func (d *libopusDecoder) Decode(packet []byte, out []float32) (int, error) {
	if len(packet) == 0 {
		return d.Conceal(out[:FrameSamples])
	}
	return d.decode(packet, out, min(len(out), 6*FrameSamples), false)
}

func (d *libopusDecoder) DecodeFEC(next []byte, out []float32) (int, error) {
	return d.decode(next, out, len(out), true)
}

func (d *libopusDecoder) Conceal(out []float32) (int, error) {
	return d.decode(nil, out, len(out), false)
}

func (d *libopusDecoder) HasFEC() bool { return true }

func (d *libopusDecoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != 0 {
		onPinned(func() { opuscc.Opus_opus_decoder_destroy(d.tls, d.st) })
		d.st = 0
		d.in.free(d.tls)
		d.out.free(d.tls)
		opuscc.FreePseudostackTLS(d.tls)
		d.tls.Close()
	}
	return nil
}

// cBuf is memory the transpiled code can be handed an address to. A Go
// slice can't be: as a uintptr it is hidden from escape analysis, may live
// on a goroutine stack, and the transpiled code's deep calls can move that
// stack mid-call. After opus-go's own cbuf.go (BSD-3-Clause).
type cBuf struct {
	p uintptr
	n int
}

func (b *cBuf) ensure(tls *libc.TLS, n int) uintptr {
	if n > b.n {
		b.free(tls)
		b.p = libc.Xmalloc(tls, uint64(n))
		b.n = n
	}
	return b.p
}

func (b *cBuf) free(tls *libc.TLS) {
	if b.p != 0 {
		libc.Xfree(tls, b.p)
	}
	b.p, b.n = 0, 0
}

// cSlice views n elements of T at p, memory from a cBuf (the shim's
// malloc, which never moves). The double indirection keeps vet quiet about
// turning a uintptr into a pointer, which is safe here for that reason.
func cSlice[T any](p uintptr, n int) []T { return unsafe.Slice(*(**T)(unsafe.Pointer(&p)), n) }

func copyIn[T any](tls *libc.TLS, b *cBuf, src []T) uintptr {
	var zero T
	p := b.ensure(tls, len(src)*int(unsafe.Sizeof(zero)))
	copy(cSlice[T](p, len(src)), src)
	return p
}
