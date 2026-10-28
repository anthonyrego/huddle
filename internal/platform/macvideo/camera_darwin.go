package macvideo

// The camera: an AVCaptureSession on its own serial queue, asking for NV12
// (420v) at 1280×720 when the camera offers it, late frames discarded.
// Each frame goes on as a retained Buffer, the camera's own memory: the
// encoders take it without a copy.
//
// Adapted from reel's internal/mac/camera.go and devices.go.

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// ErrNotBundle is a camera asked for outside Huddle.app.
var ErrNotBundle = errors.New("only Huddle.app can use the camera (make run)")

// ErrNoCameraAccess is a camera opened without the Camera permission:
// opening one macOS hasn't asked about would show its prompt, and prompts
// come only from the app's Camera button.
var ErrNoCameraAccess = errors.New("huddle doesn't have the Camera permission")

// Camera is a camera the device menu lists.
type Camera struct {
	UID     string // AVCaptureDevice's uniqueID, stable across launches
	Name    string
	Kind    CameraKind
	Default bool // the system's default camera
}

// CameraKind is what sort of camera it is.
type CameraKind int

const (
	CameraBuiltIn    CameraKind = iota // the Mac's own
	CameraExternal                     // USB and the like
	CameraContinuity                   // an iPhone (Continuity Camera)
)

func (k CameraKind) String() string {
	switch k {
	case CameraBuiltIn:
		return "built-in"
	case CameraContinuity:
		return "iPhone"
	}
	return "USB"
}

// The Info.plist keys the camera needs: macOS kills a process that opens a
// camera without the first (no exception, no error), and lists iPhones
// only to apps with the second.
const (
	cameraUsageKey      = "NSCameraUsageDescription"
	continuityCameraKey = "NSCameraUseContinuityCameraDeviceType"
)

// AVAuthorizationStatus values (AVCaptureDevice.h).
const (
	avNotDetermined = 0
	avAuthorized    = 3
)

// CameraAccess reports the Camera permission without asking.
func CameraAccess() Permission {
	if loadObjC() != nil {
		return PermissionDenied
	}
	var st int
	withPool(func() {
		st = objc.Send[int](class("AVCaptureDevice"), sel("authorizationStatusForMediaType:"), constant("AVMediaTypeVideo"))
	})
	switch st {
	case avAuthorized:
		return PermissionGranted
	case avNotDetermined:
		return PermissionUnknown
	}
	return PermissionDenied
}

// RequestCameraAccess asks for the Camera permission: macOS shows its
// prompt the first time and answers from the stored choice after. done
// gets the answer on an AVFoundation queue. It opens no device. Only
// Huddle.app asks: a bare binary would be asking for its terminal.
func RequestCameraAccess(done func(granted bool)) error {
	if err := loadObjC(); err != nil {
		return err
	}
	if !InBundle() {
		return ErrNotBundle
	}
	var block objc.Block
	block = objc.NewBlock(func(_ objc.Block, granted bool) {
		done(granted)
		block.Release()
	})
	withPool(func() {
		class("AVCaptureDevice").Send(sel("requestAccessForMediaType:completionHandler:"), constant("AVMediaTypeVideo"), block)
	})
	return nil
}

// InBundle reports whether this process is Huddle.app (a bundle
// identifier and the camera's usage string), not the bare binary.
func InBundle() bool {
	if loadObjC() != nil {
		return false
	}
	var ok bool
	withPool(func() {
		b := class("NSBundle").Send(sel("mainBundle"))
		ok = b != 0 && goString(b.Send(sel("bundleIdentifier"))) != "" &&
			b.Send(sel("objectForInfoDictionaryKey:"), nsString(cameraUsageKey)) != 0
	})
	return ok
}

// infoBool reads a boolean from the main bundle's Info.plist (inside
// withPool).
func infoBool(key string) bool {
	b := class("NSBundle").Send(sel("mainBundle"))
	if b == 0 {
		return false
	}
	v := b.Send(sel("objectForInfoDictionaryKey:"), nsString(key))
	return v != 0 && objc.Send[bool](v, sel("boolValue"))
}

// Cameras lists the cameras through discovery sessions. Listing opens
// nothing: no prompt, no light. iPhones are listed only in Huddle.app,
// whose Info.plist asks for them.
func Cameras() ([]Camera, error) {
	if err := loadObjC(); err != nil {
		return nil, err
	}
	var cams []Camera
	withPool(func() {
		def := ""
		if d := class("AVCaptureDevice").Send(sel("defaultDeviceWithMediaType:"), constant("AVMediaTypeVideo")); d != 0 {
			def = goString(d.Send(sel("uniqueID")))
		}
		type kind struct {
			deviceType string
			kind       CameraKind
		}
		kinds := []kind{{"AVCaptureDeviceTypeBuiltInWideAngleCamera", CameraBuiltIn}, {"AVCaptureDeviceTypeExternal", CameraExternal}}
		if infoBool(continuityCameraKey) {
			kinds = append(kinds, kind{"AVCaptureDeviceTypeContinuityCamera", CameraContinuity})
		}
		seen := map[string]bool{}
		// One session per type, so each camera's kind is the session's.
		for _, k := range kinds {
			s := class("AVCaptureDeviceDiscoverySession").Send(sel("discoverySessionWithDeviceTypes:mediaType:position:"),
				nsArray(constant(k.deviceType)), constant("AVMediaTypeVideo"), 0)
			each(s.Send(sel("devices")), func(d objc.ID) {
				uid := goString(d.Send(sel("uniqueID")))
				if seen[uid] || !objc.Send[bool](d, sel("isConnected")) {
					return
				}
				seen[uid] = true
				cams = append(cams, Camera{UID: uid, Name: goString(d.Send(sel("localizedName"))), Kind: k.kind, Default: uid == def})
			})
		}
	})
	return cams, nil
}

// FrameFunc gets each picture, which it owns (it releases it), and when
// it was taken. It runs on the capture's own queue or goroutine.
type FrameFunc func(b *Buffer, at time.Time)

// CameraStream is a running camera.
type CameraStream struct {
	session, delegate objc.ID
	observers         []objc.ID
	fn                FrameFunc

	mu      sync.Mutex
	stopped bool
	onStop  func(error)
}

// cameraStreams finds the camera a delegate object belongs to: a frame
// still queued from a stopped camera finds none and is dropped.
var cameraStreams = struct {
	sync.Mutex
	m map[objc.ID]*CameraStream
}{m: map[objc.ID]*CameraStream{}}

var cameraOutput struct {
	once  sync.Once
	class objc.Class
	err   error
	queue uintptr
}

// cameraQueueLabel lives as long as the process; libdispatch keeps a
// pointer to it.
var cameraQueueLabel = []byte("com.anthonyrego.huddle.camera\x00")

// loadCamera registers the camera's delegate class and makes its queue,
// once.
func loadCamera() error {
	cameraOutput.once.Do(func() {
		if cameraOutput.err = loadObjC(); cameraOutput.err != nil {
			return
		}
		if cameraOutput.queue, cameraOutput.err = newQueue(cameraQueueLabel); cameraOutput.err != nil {
			return
		}
		protos := []*objc.Protocol{protocol("AVCaptureVideoDataOutputSampleBufferDelegate",
			[2]string{"captureOutput:didOutputSampleBuffer:fromConnection:", "v@:@^v@"})}
		cameraOutput.class, cameraOutput.err = objc.RegisterClass("HuddleCameraOutput", objc.GetClass("NSObject"), protos, nil, []objc.MethodDef{
			{Cmd: sel("captureOutput:didOutputSampleBuffer:fromConnection:"), Fn: cameraDidOutput},
		})
	})
	return cameraOutput.err
}

// cameraDidOutput is captureOutput:didOutputSampleBuffer:fromConnection:,
// on the camera's queue: C calls only (no message sends per frame).
func cameraDidOutput(self objc.ID, _ objc.SEL, _ objc.ID, sbuf uintptr, _ objc.ID) {
	cameraStreams.Lock()
	c := cameraStreams.m[self]
	cameraStreams.Unlock()
	if c == nil || sbuf == 0 {
		return
	}
	img := oc.imageBuffer(sbuf)
	if img == 0 {
		return
	}
	c.fn(retainBuffer(img), time.Now())
}

// StartCamera opens a camera (a UID from Cameras; "" for the default) and
// starts its frames. It blocks until the camera runs (about a second):
// call it off the main thread. onStop (may be nil) hears once if the
// camera stops by itself (unplugged, taken), with why.
//
// It refuses outside Huddle.app (macOS kills a process without the usage
// string the moment it opens a camera) and without the Camera permission
// (opening one would show the prompt): an agent's bare binary never lights
// the camera.
func StartCamera(uid string, fn FrameFunc, onStop func(error)) (*CameraStream, error) {
	if err := loadCamera(); err != nil {
		return nil, fmt.Errorf("camera: %w", err)
	}
	if !InBundle() {
		return nil, ErrNotBundle
	}
	if CameraAccess() != PermissionGranted {
		return nil, ErrNoCameraAccess
	}
	c := &CameraStream{fn: fn, onStop: onStop}
	var err error
	withPool(func() { err = c.start(uid) })
	if err != nil {
		c.release()
		return nil, err
	}
	return c, nil
}

func (c *CameraStream) start(uid string) error {
	var device objc.ID
	if uid != "" {
		device = class("AVCaptureDevice").Send(sel("deviceWithUniqueID:"), nsString(uid))
	}
	if device == 0 {
		device = class("AVCaptureDevice").Send(sel("defaultDeviceWithMediaType:"), constant("AVMediaTypeVideo"))
	}
	if device == 0 {
		return errors.New("camera: no camera")
	}
	var nserr objc.ID
	input := class("AVCaptureDeviceInput").Send(sel("deviceInputWithDevice:error:"), device, unsafe.Pointer(&nserr))
	if input == 0 {
		return nsError("camera: open", nserr)
	}
	c.session = class("AVCaptureSession").Send(sel("alloc")).Send(sel("init"))
	c.session.Send(sel("beginConfiguration"))
	if preset := constant("AVCaptureSessionPreset1280x720"); objc.Send[bool](c.session, sel("canSetSessionPreset:"), preset) {
		c.session.Send(sel("setSessionPreset:"), preset)
	}
	if !objc.Send[bool](c.session, sel("canAddInput:"), input) {
		c.session.Send(sel("commitConfiguration"))
		return errors.New("camera: the session won't take the camera")
	}
	c.session.Send(sel("addInput:"), input)

	out := class("AVCaptureVideoDataOutput").Send(sel("alloc")).Send(sel("init")).Send(sel("autorelease"))
	nv12 := false
	each(out.Send(sel("availableVideoCVPixelFormatTypes")), func(n objc.ID) {
		nv12 = nv12 || objc.Send[uint32](n, sel("unsignedIntValue")) == formatNV12
	})
	if !nv12 {
		c.session.Send(sel("commitConfiguration"))
		return errors.New("camera: the camera's output has no 420v")
	}
	// Setting a format the output doesn't offer would throw: checked above.
	// Mirroring is the window's (video.Frame.Mirror): the connection's
	// throws unless its automatic adjustment is turned off first.
	out.Send(sel("setVideoSettings:"), nsDict(objc.ID(k("kCVPixelBufferPixelFormatTypeKey")),
		class("NSNumber").Send(sel("numberWithLongLong:"), int64(formatNV12))))
	out.Send(sel("setAlwaysDiscardsLateVideoFrames:"), true)
	c.delegate = objc.ID(cameraOutput.class).Send(sel("alloc")).Send(sel("init"))
	cameraStreams.Lock()
	cameraStreams.m[c.delegate] = c
	cameraStreams.Unlock()
	out.Send(sel("setSampleBufferDelegate:queue:"), c.delegate, cameraOutput.queue)
	if !objc.Send[bool](c.session, sel("canAddOutput:"), out) {
		c.session.Send(sel("commitConfiguration"))
		return errors.New("camera: the session won't take the output")
	}
	c.session.Send(sel("addOutput:"), out)
	c.session.Send(sel("commitConfiguration"))

	// Unplugged, or a runtime error (another app took the camera, the
	// system reclaimed it): the camera has stopped.
	center := class("NSNotificationCenter").Send(sel("defaultCenter"))
	for _, w := range []struct {
		name   string
		object objc.ID
	}{{"AVCaptureDeviceWasDisconnectedNotification", device}, {"AVCaptureSessionRuntimeErrorNotification", c.session}} {
		name := w.name
		block := objc.NewBlock(func(_ objc.Block, note objc.ID) {
			err := errors.New("camera: unplugged")
			if name == "AVCaptureSessionRuntimeErrorNotification" {
				err = nsError("camera: stopped", note.Send(sel("userInfo")).Send(sel("objectForKey:"), constant("AVCaptureSessionErrorKey")))
			}
			slog.Warn("camera: stopped by itself", "err", err)
			// Not on the notification's thread, which may be the session's
			// own: stopping from there could wait on itself.
			go c.finish(err)
		})
		obs := center.Send(sel("addObserverForName:object:queue:usingBlock:"), constant(name), w.object, objc.ID(0), block)
		c.observers = append(c.observers, obs.Send(sel("retain")))
	}

	c.session.Send(sel("startRunning"))
	if !objc.Send[bool](c.session, sel("isRunning")) {
		return errors.New("camera: it wouldn't start")
	}
	return nil
}

// Stop stops the camera (blocking, like StartCamera). Frames still queued
// are dropped.
func (c *CameraStream) Stop() {
	c.mu.Lock()
	c.onStop = nil
	c.mu.Unlock()
	c.finish(nil)
}

// finish tells onStop once and lets the camera go.
func (c *CameraStream) finish(err error) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	onStop := c.onStop
	c.mu.Unlock()
	c.release()
	if onStop != nil {
		onStop(err)
	}
}

func (c *CameraStream) release() {
	if c.delegate != 0 {
		cameraStreams.Lock()
		delete(cameraStreams.m, c.delegate)
		cameraStreams.Unlock()
	}
	withPool(func() {
		center := class("NSNotificationCenter").Send(sel("defaultCenter"))
		for _, o := range c.observers {
			center.Send(sel("removeObserver:"), o)
			o.Send(sel("release"))
		}
		c.observers = nil
		if c.session != 0 {
			if objc.Send[bool](c.session, sel("isRunning")) {
				c.session.Send(sel("stopRunning"))
			}
			c.session.Send(sel("release"))
			c.session = 0
		}
	})
	// The observers' blocks may be running (one may be why we're here) and
	// the output may still hold the delegate while it drains its queue:
	// both are a few bytes per camera opened, never released.
}
