package macvideo

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/anthonyrego/huddle/internal/video"
)

// EncoderConfig sets up an H.264 encoder.
type EncoderConfig struct {
	W, H     int
	FPS      int           // the frame rate expected (30)
	Bitrate  int           // bits per second, on average (1 Mbps)
	KeyEvery time.Duration // the longest between keyframes (2 s)
	// NoLowLatency skips VideoToolbox's low-latency rate control (made for
	// calls: no frame waits on later ones, and the rate holds frame to
	// frame); without it, or where it isn't offered, the encoder is the
	// ordinary real-time one.
	NoLowLatency bool
}

// Encoded is one access unit out of the encoder.
type Encoded struct {
	Data []byte // Annex-B; a keyframe starts with its SPS and PPS
	Key  bool
	PTS  time.Duration
	Seq  uint64        // which Encode call it came from
	Took time.Duration // from Encode to here
}

// Encoder is a VTCompressionSession making H.264 for the network:
// Constrained Baseline, no frame reordering, real time.
type Encoder struct {
	cfg EncoderConfig
	s   uintptr
	id  uintptr
	out func(Encoded)

	// LowLatency is whether the low-latency rate control took.
	LowLatency bool
	// Warnings are properties the encoder turned down.
	Warnings []string

	mu      sync.Mutex
	seq     uint64
	sent    map[uint64]sentFrame
	dropped int
	// The time taken out of the timestamps VideoToolbox is given (Skipped),
	// the newest timestamp handed over, and whether the next closes a gap.
	shift   time.Duration
	last    time.Duration
	skipped bool
}

// The encoders, by the id their output callback gets.
var encoders = struct {
	sync.Mutex
	m    map[uintptr]*Encoder
	next uintptr
}{m: map[uintptr]*Encoder{}}

// encodeCB is the one output callback every encoder shares (purego never
// frees callbacks, so they're made once).
var encodeCB = sync.OnceValue(func() uintptr { return purego.NewCallback(encodeOutput) })

const infoFrameDropped = 1 << 1 // kVTEncodeInfo_FrameDropped

// couldNotFindEncoder is kVTCouldNotFindVideoEncoderErr.
const couldNotFindEncoder = -12908

// NewEncoder starts an encoder; out gets every access unit, on
// VideoToolbox's own thread (keep it quick: hand off and return).
func NewEncoder(cfg EncoderConfig, out func(Encoded)) (*Encoder, error) {
	if err := load(); err != nil {
		return nil, err
	}
	if cfg.W <= 0 || cfg.H <= 0 || cfg.W%2 != 0 || cfg.H%2 != 0 {
		return nil, fmt.Errorf("encoder: %dx%d isn't an even size", cfg.W, cfg.H)
	}
	if cfg.FPS == 0 {
		cfg.FPS = 30
	}
	if cfg.Bitrate == 0 {
		cfg.Bitrate = 1_000_000
	}
	if cfg.KeyEvery == 0 {
		cfg.KeyEvery = 2 * time.Second
	}
	e := &Encoder{cfg: cfg, out: out, sent: map[uint64]sentFrame{}}
	encoders.Lock()
	encoders.next++
	e.id = encoders.next
	encoders.m[e.id] = e
	encoders.Unlock()

	src := cfDict(
		k("kCVPixelBufferPixelFormatTypeKey"), cfInt(int64(formatNV12)),
		k("kCVPixelBufferWidthKey"), cfInt(int64(cfg.W)),
		k("kCVPixelBufferHeightKey"), cfInt(int64(cfg.H)),
		k("kCVPixelBufferIOSurfacePropertiesKey"), cfDict())
	defer cf.release(src)
	create := func(lowLatency bool) int32 {
		var spec uintptr
		if lowLatency {
			spec = cfDict(k("kVTVideoEncoderSpecification_EnableLowLatencyRateControl"), cfBool(true))
			defer cf.release(spec)
		}
		return vt.encCreate(0, int32(cfg.W), int32(cfg.H), codecH264, spec, src, 0, encodeCB(), e.id, &e.s)
	}
	st := int32(-1)
	if !cfg.NoLowLatency {
		if st = create(true); st == 0 {
			e.LowLatency = true
		}
	}
	if st != 0 {
		st = create(false)
	}
	if err := check("VTCompressionSessionCreate", st); err != nil {
		e.unregister()
		return nil, err
	}
	e.set("RealTime", k("kVTCompressionPropertyKey_RealTime"), cfBool(true), true)
	e.set("AllowFrameReordering", k("kVTCompressionPropertyKey_AllowFrameReordering"), cfBool(false), true)
	if !e.set("ProfileLevel", k("kVTCompressionPropertyKey_ProfileLevel"), k("kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel"), false) {
		e.set("ProfileLevel (Baseline)", k("kVTCompressionPropertyKey_ProfileLevel"), k("kVTProfileLevel_H264_Baseline_AutoLevel"), false)
	}
	e.set("ExpectedFrameRate", k("kVTCompressionPropertyKey_ExpectedFrameRate"), cfInt(int64(cfg.FPS)), true)
	e.set("MaxKeyFrameIntervalDuration", k("kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration"), cfFloat(cfg.KeyEvery.Seconds()), true)
	e.set("MaxKeyFrameInterval", k("kVTCompressionPropertyKey_MaxKeyFrameInterval"), cfInt(int64(cfg.KeyEvery.Seconds()*float64(cfg.FPS))), true)
	e.SetBitrate(cfg.Bitrate)
	if err := check("VTCompressionSessionPrepareToEncodeFrames", vt.encPrepare(e.s)); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

// set sets a session property, releasing an owned value; a refusal is kept
// in Warnings.
func (e *Encoder) set(name string, key, value uintptr, owned bool) bool {
	st := vt.setProperty(e.s, key, value)
	if owned {
		cf.release(value)
	}
	if st != 0 {
		e.Warnings = append(e.Warnings, fmt.Sprintf("%s: OSStatus %d", name, st))
	}
	return st == 0
}

// SetBitrate changes the average bitrate, and the limit that keeps any one
// second within half again of it.
func (e *Encoder) SetBitrate(bps int) {
	e.set("AverageBitRate", k("kVTCompressionPropertyKey_AverageBitRate"), cfInt(int64(bps)), true)
	limit := cfArray(cfInt(int64(bps*3/2/8)), cfFloat(1)) // bytes per one second
	e.set("DataRateLimits", k("kVTCompressionPropertyKey_DataRateLimits"), limit, true)
}

// NewBuffer is a buffer from the encoder's own pool, the size it encodes.
func (e *Encoder) NewBuffer() (*Buffer, error) {
	pool := vt.encPool(e.s)
	if pool == 0 {
		return NewBuffer(e.cfg.W, e.cfg.H)
	}
	var ref uintptr
	if err := check("CVPixelBufferPoolCreatePixelBuffer", cv.poolCreateBuf(0, pool, &ref)); err != nil {
		return nil, err
	}
	return &Buffer{ref}, nil
}

// Encode hands a picture to the encoder; its access unit comes to out.
// PTS must rise from call to call. forceKey asks for a keyframe (for a
// receiver's PLI).
func (e *Encoder) Encode(b *Buffer, pts time.Duration, forceKey bool) error {
	frame := time.Second / time.Duration(e.cfg.FPS)
	e.mu.Lock()
	if e.skipped && e.seq > 0 && pts-e.last > frame {
		e.shift += pts - e.last - frame
	}
	e.skipped, e.last = false, pts
	e.seq++
	seq := e.seq
	e.sent[seq] = sentFrame{time.Now(), pts}
	given := pts - e.shift
	e.mu.Unlock()
	t := cmTime{Value: given.Microseconds(), Timescale: 1_000_000, Flags: cmTimeValid}
	d := cmTime{Value: int64(frame / time.Microsecond), Timescale: 1_000_000, Flags: cmTimeValid}
	var props uintptr
	if forceKey {
		props = cfDict(k("kVTEncodeFrameOptionKey_ForceKeyFrame"), cfBool(true))
		defer cf.release(props)
	}
	var flags uint32
	if err := check("VTCompressionSessionEncodeFrame", vt.encFrame(e.s, b.ref, t, d, props, uintptr(seq), &flags)); err != nil {
		e.mu.Lock()
		delete(e.sent, seq)
		e.mu.Unlock()
		return err
	}
	return nil
}

// Skipped says the encoder is given no pictures for a while on purpose
// (its layer is paused). VideoToolbox's rate control counts a gap in the
// timestamps as bits not spent, and spends them as soon as pictures come
// again: a 720p encoder at 1.7 Mbps, skipped for 6 s and set to 680 kbps,
// sent 1.7–1.9 Mbps for its next 4 s (with the gap taken out, about 1.06
// Mbps from the first second, as one never skipped). So the next picture's
// timestamp, as VideoToolbox is given it, follows the last one's by a
// frame, and every later one moves back as much; what comes out keeps the
// timestamps Encode was given.
func (e *Encoder) Skipped() {
	e.mu.Lock()
	e.skipped = true
	e.mu.Unlock()
}

// Flush waits for every frame handed over to come out.
func (e *Encoder) Flush() error {
	return check("VTCompressionSessionCompleteFrames", vt.encComplete(e.s, cmTime{}))
}

// Dropped is how many frames the encoder dropped or failed.
func (e *Encoder) Dropped() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// Close flushes and ends the session.
func (e *Encoder) Close() {
	if e.s != 0 {
		e.Flush()
		vt.encInvalidate(e.s)
		cf.release(e.s)
		e.s = 0
	}
	e.unregister()
}

func (e *Encoder) unregister() {
	encoders.Lock()
	delete(encoders.m, e.id)
	encoders.Unlock()
}

// encodeOutput is VideoToolbox's output callback, on its thread.
func encodeOutput(refcon, frameRef uintptr, status int32, flags uint32, sbuf uintptr) {
	encoders.Lock()
	e := encoders.m[refcon]
	encoders.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	sf, ok := e.sent[uint64(frameRef)]
	delete(e.sent, uint64(frameRef))
	e.mu.Unlock()
	if status != 0 || sbuf == 0 || flags&infoFrameDropped != 0 {
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
		return
	}
	key := isSync(sbuf)
	au, err := annexB(sbuf, key)
	if err != nil {
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
		return
	}
	enc := Encoded{Data: au, Key: key, Seq: uint64(frameRef)}
	if ok {
		enc.Took, enc.PTS = time.Since(sf.at), sf.pts
	}
	e.out(enc)
}

// isSync reports whether a sample is a keyframe: one whose attachments
// don't say NotSync.
func isSync(sbuf uintptr) bool {
	arr := cm.attachments(sbuf, false)
	if arr == 0 || cf.arrayCount(arr) == 0 {
		return true
	}
	v := cf.dictGet(cf.arrayAt(arr, 0), k("kCMSampleAttachmentKey_NotSync"))
	return v == 0 || v == k("kCFBooleanFalse")
}

// annexB copies an encoded sample out as Annex-B, a keyframe's parameter
// sets ahead of it.
func annexB(sbuf uintptr, key bool) ([]byte, error) {
	bb := cm.dataBuffer(sbuf)
	if bb == 0 {
		return nil, errors.New("no data")
	}
	n := cm.blockLen(bb)
	if n == 0 {
		return nil, errors.New("empty")
	}
	avcc := make([]byte, n)
	if err := check("CMBlockBufferCopyDataBytes", cm.blockCopy(bb, 0, n, unsafe.Pointer(&avcc[0]))); err != nil {
		return nil, err
	}
	out := make([]byte, 0, n+64)
	if key {
		fd := cm.formatDesc(sbuf)
		var p unsafe.Pointer
		var size, count uintptr
		var hdr int32
		if err := check("parameter sets", cm.paramSet(fd, 0, &p, &size, &count, &hdr)); err != nil {
			return nil, err
		}
		if hdr != 4 {
			return nil, fmt.Errorf("NAL lengths of %d bytes", hdr)
		}
		for i := range int(count) {
			if err := check("parameter set", cm.paramSet(fd, i, &p, &size, nil, nil)); err != nil {
				return nil, err
			}
			out = video.AppendAnnexB(out, unsafe.Slice((*byte)(p), size))
		}
	}
	return video.AVCCToAnnexB(out, avcc)
}

// sentFrame is a frame handed to the encoder: when, and its pts.
type sentFrame struct {
	at  time.Time
	pts time.Duration
}

// Size is the size the encoder makes.
func (e *Encoder) Size() (w, h int) { return e.cfg.W, e.cfg.H }
