// Package macvideo is the macOS side of the app's video: CoreVideo pixel
// buffers, VideoToolbox's H.264 encoder and decoder, and scaling, called
// through purego (no cgo). Frames cross into Go memory as video.Frame.
//
// It is all C functions and CoreFoundation types: nothing here can raise
// an Objective-C exception. Every function and constant it uses is in the
// tables below, resolved once by load and checked by the tests without
// touching a device. Framework constants are read through Dlsym, never
// hard-coded.
package macvideo

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	coreFoundationPath = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	coreMediaPath      = "/System/Library/Frameworks/CoreMedia.framework/CoreMedia"
	coreVideoPath      = "/System/Library/Frameworks/CoreVideo.framework/CoreVideo"
	videoToolboxPath   = "/System/Library/Frameworks/VideoToolbox.framework/VideoToolbox"
)

// constants are the framework constants (extern CFStringRef and the like)
// the package reads, by library.
var constants = map[string][]string{
	coreFoundationPath: {"kCFBooleanTrue", "kCFBooleanFalse"},
	coreVideoPath: {"kCVPixelBufferPixelFormatTypeKey", "kCVPixelBufferWidthKey", "kCVPixelBufferHeightKey",
		"kCVPixelBufferIOSurfacePropertiesKey"},
	coreMediaPath: {"kCMSampleAttachmentKey_NotSync"},
	videoToolboxPath: {"kVTCompressionPropertyKey_RealTime", "kVTCompressionPropertyKey_ProfileLevel",
		"kVTCompressionPropertyKey_AllowFrameReordering", "kVTCompressionPropertyKey_AverageBitRate",
		"kVTCompressionPropertyKey_DataRateLimits", "kVTCompressionPropertyKey_MaxKeyFrameInterval",
		"kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration", "kVTCompressionPropertyKey_ExpectedFrameRate",
		"kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel", "kVTProfileLevel_H264_Baseline_AutoLevel",
		"kVTVideoEncoderSpecification_EnableLowLatencyRateControl",
		"kVTEncodeFrameOptionKey_ForceKeyFrame", "kVTDecompressionPropertyKey_RealTime"},
}

// addresses are framework globals the package needs the address of (a
// struct passed by pointer), not the value.
var addresses = map[string][]string{
	coreFoundationPath: {"kCFTypeDictionaryKeyCallBacks", "kCFTypeDictionaryValueCallBacks", "kCFTypeArrayCallBacks"},
}

var lib struct {
	once sync.Once
	err  error
	k    map[string]uintptr // constants' values and addresses' addresses
}

var cf struct {
	release      func(uintptr)
	retain       func(uintptr) uintptr
	numberCreate func(alloc uintptr, typ int, v unsafe.Pointer) uintptr
	dictCreate   func(alloc uintptr, keys, vals unsafe.Pointer, n int, kcb, vcb uintptr) uintptr
	dictGet      func(d, key uintptr) uintptr
	arrayCreate  func(alloc uintptr, vals unsafe.Pointer, n int, cb uintptr) uintptr
	arrayCount   func(uintptr) int
	arrayAt      func(uintptr, int) uintptr
}

var cv struct {
	create        func(alloc uintptr, w, h int, format uint32, attrs uintptr, out *uintptr) int32
	poolCreateBuf func(alloc, pool uintptr, out *uintptr) int32
	width         func(uintptr) int
	height        func(uintptr) int
	format        func(uintptr) uint32
	lock          func(buf uintptr, flags uint64) int32
	unlock        func(buf uintptr, flags uint64) int32
	planeBase     func(buf uintptr, plane int) unsafe.Pointer
	planeStride   func(buf uintptr, plane int) int
	planeCount    func(buf uintptr) int
}

var cm struct {
	dataBuffer   func(sbuf uintptr) uintptr
	formatDesc   func(sbuf uintptr) uintptr
	attachments  func(sbuf uintptr, create bool) uintptr
	blockLen     func(bb uintptr) int
	blockCopy    func(bb uintptr, offset, n int, dst unsafe.Pointer) int32
	blockCreate  func(alloc, mem uintptr, n int, blockAlloc, custom uintptr, offset, dataLen int, flags uint32, out *uintptr) int32
	blockReplace func(src unsafe.Pointer, bb uintptr, offset, n int) int32
	sampleCreate func(alloc, bb, fd uintptr, nSamples, nTiming int, timing uintptr, nSizes int, sizes *uintptr, out *uintptr) int32
	paramSet     func(fd uintptr, i int, ptr *unsafe.Pointer, size *uintptr, count *uintptr, hdrLen *int32) int32
	fdFromParams func(alloc uintptr, n int, ptrs *unsafe.Pointer, sizes *uintptr, hdrLen int32, out *uintptr) int32
	fdDimensions func(fd uintptr) uint64
	fdEqual      func(a, b uintptr) bool
}

var vt struct {
	encCreate      func(alloc uintptr, w, h int32, codec uint32, spec, srcAttrs, dataAlloc, cb, refcon uintptr, out *uintptr) int32
	encPrepare     func(s uintptr) int32
	encFrame       func(s, image uintptr, pts, dur cmTime, props, refcon uintptr, flags *uint32) int32
	encComplete    func(s uintptr, until cmTime) int32
	encInvalidate  func(s uintptr)
	encPool        func(s uintptr) uintptr
	setProperty    func(s, key, value uintptr) int32
	decCreate      func(alloc, fd, spec, dstAttrs uintptr, cb *decodeRecord, out *uintptr) int32
	decFrame       func(s, sbuf uintptr, flags uint32, refcon uintptr, infoOut *uint32) int32
	decInvalidate  func(s uintptr)
	decCanAccept   func(s, fd uintptr) bool
	xferCreate     func(alloc uintptr, out *uintptr) int32
	xferImage      func(s, src, dst uintptr) int32
	xferInvalidate func(s uintptr)
}

// funcs binds every C function the package calls: library, name, where.
var funcs = []struct {
	lib, name string
	fn        any
}{
	{coreFoundationPath, "CFRelease", &cf.release},
	{coreFoundationPath, "CFRetain", &cf.retain},
	{coreFoundationPath, "CFNumberCreate", &cf.numberCreate},
	{coreFoundationPath, "CFDictionaryCreate", &cf.dictCreate},
	{coreFoundationPath, "CFDictionaryGetValue", &cf.dictGet},
	{coreFoundationPath, "CFArrayCreate", &cf.arrayCreate},
	{coreFoundationPath, "CFArrayGetCount", &cf.arrayCount},
	{coreFoundationPath, "CFArrayGetValueAtIndex", &cf.arrayAt},

	{coreVideoPath, "CVPixelBufferCreate", &cv.create},
	{coreVideoPath, "CVPixelBufferPoolCreatePixelBuffer", &cv.poolCreateBuf},
	{coreVideoPath, "CVPixelBufferGetWidth", &cv.width},
	{coreVideoPath, "CVPixelBufferGetHeight", &cv.height},
	{coreVideoPath, "CVPixelBufferGetPixelFormatType", &cv.format},
	{coreVideoPath, "CVPixelBufferLockBaseAddress", &cv.lock},
	{coreVideoPath, "CVPixelBufferUnlockBaseAddress", &cv.unlock},
	{coreVideoPath, "CVPixelBufferGetBaseAddressOfPlane", &cv.planeBase},
	{coreVideoPath, "CVPixelBufferGetBytesPerRowOfPlane", &cv.planeStride},
	{coreVideoPath, "CVPixelBufferGetPlaneCount", &cv.planeCount},

	{coreMediaPath, "CMSampleBufferGetDataBuffer", &cm.dataBuffer},
	{coreMediaPath, "CMSampleBufferGetFormatDescription", &cm.formatDesc},
	{coreMediaPath, "CMSampleBufferGetSampleAttachmentsArray", &cm.attachments},
	{coreMediaPath, "CMBlockBufferGetDataLength", &cm.blockLen},
	{coreMediaPath, "CMBlockBufferCopyDataBytes", &cm.blockCopy},
	{coreMediaPath, "CMBlockBufferCreateWithMemoryBlock", &cm.blockCreate},
	{coreMediaPath, "CMBlockBufferReplaceDataBytes", &cm.blockReplace},
	{coreMediaPath, "CMSampleBufferCreateReady", &cm.sampleCreate},
	{coreMediaPath, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex", &cm.paramSet},
	{coreMediaPath, "CMVideoFormatDescriptionCreateFromH264ParameterSets", &cm.fdFromParams},
	{coreMediaPath, "CMVideoFormatDescriptionGetDimensions", &cm.fdDimensions},
	{coreMediaPath, "CMFormatDescriptionEqual", &cm.fdEqual},

	{videoToolboxPath, "VTCompressionSessionCreate", &vt.encCreate},
	{videoToolboxPath, "VTCompressionSessionPrepareToEncodeFrames", &vt.encPrepare},
	{videoToolboxPath, "VTCompressionSessionEncodeFrame", &vt.encFrame},
	{videoToolboxPath, "VTCompressionSessionCompleteFrames", &vt.encComplete},
	{videoToolboxPath, "VTCompressionSessionInvalidate", &vt.encInvalidate},
	{videoToolboxPath, "VTCompressionSessionGetPixelBufferPool", &vt.encPool},
	{videoToolboxPath, "VTSessionSetProperty", &vt.setProperty},
	{videoToolboxPath, "VTDecompressionSessionCreate", &vt.decCreate},
	{videoToolboxPath, "VTDecompressionSessionDecodeFrame", &vt.decFrame},
	{videoToolboxPath, "VTDecompressionSessionInvalidate", &vt.decInvalidate},
	{videoToolboxPath, "VTDecompressionSessionCanAcceptFormatDescription", &vt.decCanAccept},
	{videoToolboxPath, "VTPixelTransferSessionCreate", &vt.xferCreate},
	{videoToolboxPath, "VTPixelTransferSessionTransferImage", &vt.xferImage},
	{videoToolboxPath, "VTPixelTransferSessionInvalidate", &vt.xferInvalidate},
}

// load opens the frameworks and resolves every function and constant once.
func load() error {
	lib.once.Do(func() { lib.err = loadOnce() })
	return lib.err
}

func loadOnce() error {
	handles := map[string]uintptr{}
	for _, path := range []string{coreFoundationPath, coreMediaPath, coreVideoPath, videoToolboxPath} {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return fmt.Errorf("macvideo: %w", err)
		}
		handles[path] = h
	}
	for _, f := range funcs {
		// Dlsym first: RegisterLibFunc panics on a missing symbol.
		if _, err := purego.Dlsym(handles[f.lib], f.name); err != nil {
			return fmt.Errorf("macvideo: %w", err)
		}
		purego.RegisterLibFunc(f.fn, handles[f.lib], f.name)
	}
	lib.k = map[string]uintptr{}
	for path, names := range constants {
		for _, name := range names {
			p, err := purego.Dlsym(handles[path], name)
			if err != nil {
				return fmt.Errorf("macvideo: %w", err)
			}
			v := deref(p)
			if v == 0 {
				return fmt.Errorf("macvideo: constant %s is nil", name)
			}
			lib.k[name] = v
		}
	}
	for path, names := range addresses {
		for _, name := range names {
			p, err := purego.Dlsym(handles[path], name)
			if err != nil {
				return fmt.Errorf("macvideo: %w", err)
			}
			lib.k[name] = p
		}
	}
	return nil
}

// k is a constant resolved by load.
func k(name string) uintptr {
	v, ok := lib.k[name]
	if !ok {
		panic("macvideo: " + name + " isn't in the tables")
	}
	return v
}

// deref reads the pointer stored at address p (an extern variable's
// value). Going through &p keeps vet's unsafe.Pointer check satisfied.
func deref(p uintptr) uintptr {
	return **(**uintptr)(unsafe.Pointer(&p))
}

// cmTime is CoreMedia's CMTime (24 bytes: passed by pointer on arm64,
// which purego does for a struct this size).
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const cmTimeValid = 1

// fourCC packs a four-character code.
func fourCC(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

var (
	formatNV12 = fourCC("420v") // kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange
	codecH264  = fourCC("avc1") // kCMVideoCodecType_H264
)

// CoreFoundation values. Each returns a +1 reference: the caller releases
// it (the containers retain what's put in them).

const (
	cfNumberSInt64  = 4
	cfNumberFloat64 = 6
)

func cfInt(v int64) uintptr     { return cf.numberCreate(0, cfNumberSInt64, unsafe.Pointer(&v)) }
func cfFloat(v float64) uintptr { return cf.numberCreate(0, cfNumberFloat64, unsafe.Pointer(&v)) }
func cfBool(v bool) uintptr {
	if v {
		return cf.retain(k("kCFBooleanTrue"))
	}
	return cf.retain(k("kCFBooleanFalse"))
}

// cfDict makes a dictionary of keys (constants) and values, releasing the
// values (they're +1 references, and the dictionary keeps its own).
func cfDict(pairs ...uintptr) uintptr {
	n := len(pairs) / 2
	keys, vals := make([]uintptr, max(n, 1)), make([]uintptr, max(n, 1))
	for i := range n {
		keys[i], vals[i] = pairs[2*i], pairs[2*i+1]
	}
	d := cf.dictCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&vals[0]), n,
		k("kCFTypeDictionaryKeyCallBacks"), k("kCFTypeDictionaryValueCallBacks"))
	for _, v := range vals[:n] {
		cf.release(v)
	}
	return d
}

// cfArray makes an array, releasing the values.
func cfArray(vals ...uintptr) uintptr {
	a := cf.arrayCreate(0, unsafe.Pointer(&vals[0]), len(vals), k("kCFTypeArrayCallBacks"))
	for _, v := range vals {
		cf.release(v)
	}
	return a
}

// OSStatusError is a framework call's failure.
type OSStatusError struct {
	What   string
	Status int32
}

func (e *OSStatusError) Error() string {
	return fmt.Sprintf("%s: OSStatus %d", e.What, e.Status)
}

func check(what string, st int32) error {
	if st != 0 {
		return &OSStatusError{what, st}
	}
	return nil
}
