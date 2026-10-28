package macvideo

// Capture's side of the frameworks: AVFoundation for the camera,
// ScreenCaptureKit for the screen, through purego's objc package.
//
// Calling into Objective-C has one sharp edge: an unknown selector, a nil
// in an NSDictionary or a call made in the wrong state raises an
// exception, and nothing in Go can catch it, so the process dies. Every
// selector, constant and protocol used here is in the tables below and
// checked by TestObjC without touching a device; calls that can throw are
// asked first (canAddInput:, canSetSessionPreset:, the pixel formats on
// offer). Enum values are the headers'; constants (extern NSString *) are
// always read from the frameworks, never written out.
//
// Adapted from reel's internal/mac/mac.go.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

const (
	foundationPath       = "/System/Library/Frameworks/Foundation.framework/Foundation"
	avFoundationPath     = "/System/Library/Frameworks/AVFoundation.framework/AVFoundation"
	screenCaptureKitPath = "/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit"
	coreGraphicsPath     = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"

	// callTimeout bounds a wait for a completion handler.
	callTimeout = 5 * time.Second
)

// selectors are every selector capture sends, by class; class methods are
// under "+Class".
var selectors = map[string][]string{
	"+AVCaptureDeviceDiscoverySession": {"discoverySessionWithDeviceTypes:mediaType:position:"},
	"AVCaptureDeviceDiscoverySession":  {"devices"},
	"+AVCaptureDevice":                 {"defaultDeviceWithMediaType:", "authorizationStatusForMediaType:", "requestAccessForMediaType:completionHandler:", "deviceWithUniqueID:"},
	"AVCaptureDevice":                  {"uniqueID", "localizedName", "isConnected"},
	"+AVCaptureDeviceInput":            {"deviceInputWithDevice:error:"},
	"AVCaptureSession": {"init", "beginConfiguration", "commitConfiguration", "canSetSessionPreset:", "setSessionPreset:",
		"canAddInput:", "addInput:", "canAddOutput:", "addOutput:", "startRunning", "stopRunning", "isRunning"},
	"AVCaptureVideoDataOutput": {"init", "availableVideoCVPixelFormatTypes", "setVideoSettings:", "setAlwaysDiscardsLateVideoFrames:",
		"setSampleBufferDelegate:queue:"},

	"+SCContentSharingPicker":             {"sharedPicker"},
	"SCContentSharingPicker":              {"addObserver:", "removeObserver:", "setActive:", "isActive", "present", "setDefaultConfiguration:"},
	"SCContentSharingPickerConfiguration": {"init", "setAllowedPickerModes:"},
	"SCContentFilter":                     {"contentRect", "pointPixelScale"},
	"SCStream": {"initWithFilter:configuration:delegate:", "addStreamOutput:type:sampleHandlerQueue:error:",
		"startCaptureWithCompletionHandler:", "stopCaptureWithCompletionHandler:", "updateContentFilter:completionHandler:"},
	"SCStreamConfiguration": {"init", "setWidth:", "setHeight:", "setMinimumFrameInterval:", "setPixelFormat:", "setQueueDepth:",
		"setShowsCursor:", "setColorSpaceName:", "setColorMatrix:"},

	"+NSBundle":             {"mainBundle"},
	"NSBundle":              {"bundleIdentifier", "objectForInfoDictionaryKey:"},
	"+NSArray":              {"array", "arrayWithObjects:count:"},
	"NSArray":               {"count", "objectAtIndex:"},
	"+NSString":             {"alloc"},
	"NSString":              {"initWithUTF8String:", "UTF8String"},
	"+NSObject":             {"alloc"},
	"NSObject":              {"init", "retain", "release", "autorelease", "performSelectorOnMainThread:withObject:waitUntilDone:"},
	"NSError":               {"localizedDescription", "code", "domain"},
	"NSDictionary":          {"objectForKey:"},
	"+NSDictionary":         {"dictionaryWithObjects:forKeys:count:"},
	"NSNumber":              {"unsignedIntValue", "boolValue"},
	"+NSNumber":             {"numberWithLongLong:"},
	"+NSNotificationCenter": {"defaultCenter"},
	"NSNotificationCenter":  {"addObserverForName:object:queue:usingBlock:", "removeObserver:"},
	"NSNotification":        {"userInfo"},
}

// objcConstants are the framework constants (extern NSString *const)
// capture reads, by library.
var objcConstants = map[string][]string{
	avFoundationPath: {"AVMediaTypeVideo", "AVCaptureDeviceTypeBuiltInWideAngleCamera", "AVCaptureDeviceTypeExternal",
		"AVCaptureDeviceTypeContinuityCamera", "AVCaptureSessionPreset1280x720", "AVCaptureDeviceWasDisconnectedNotification",
		"AVCaptureSessionRuntimeErrorNotification", "AVCaptureSessionErrorKey"},
	coreGraphicsPath:     {"kCGColorSpaceSRGB", "kCGDisplayStreamYCbCrMatrix_ITU_R_709_2"},
	screenCaptureKitPath: {"SCStreamFrameInfoStatus"},
}

// protocols are what huddle's own classes conform to, by class.
var protocols = map[string][]string{
	"HuddleCameraOutput":   {"AVCaptureVideoDataOutputSampleBufferDelegate"},
	"HuddleStreamOutput":   {"SCStreamOutput", "SCStreamDelegate"},
	"HuddlePickerObserver": {"SCContentSharingPickerObserver"},
}

// classMethods are the methods huddle's classes implement, by class.
var classMethods = map[string][]string{
	"HuddleCameraOutput":   {"captureOutput:didOutputSampleBuffer:fromConnection:"},
	"HuddleStreamOutput":   {"stream:didOutputSampleBuffer:ofType:", "stream:didStopWithError:"},
	"HuddlePickerObserver": {"contentSharingPicker:didCancelForStream:", "contentSharingPicker:didUpdateWithFilter:forStream:", "contentSharingPickerStartDidFailWithError:", "presentPicker:"},
}

var oc struct {
	once   sync.Once
	err    error
	consts map[string]objc.ID

	poolPush        func() uintptr
	poolPop         func(uintptr)
	queueCreate     func(label *byte, attr uintptr) uintptr
	attrAutorelease func(attr uintptr, frequency uint) uintptr
	imageBuffer     func(sbuf uintptr) uintptr
	numberGet       func(n uintptr, typ int, out unsafe.Pointer) bool
}

// loadObjC opens the capture frameworks and resolves the tables, once.
func loadObjC() error {
	oc.once.Do(func() { oc.err = loadObjCOnce() })
	return oc.err
}

func loadObjCOnce() error {
	if err := load(); err != nil {
		return err
	}
	handles := map[string]uintptr{}
	for _, path := range []string{foundationPath, coreGraphicsPath, avFoundationPath, screenCaptureKitPath} {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return fmt.Errorf("macvideo: %w", err)
		}
		handles[path] = h
	}
	for name, fn := range map[string]any{
		"objc_autoreleasePoolPush":                            &oc.poolPush,
		"objc_autoreleasePoolPop":                             &oc.poolPop,
		"dispatch_queue_create":                               &oc.queueCreate,
		"dispatch_queue_attr_make_with_autorelease_frequency": &oc.attrAutorelease,
		"CMSampleBufferGetImageBuffer":                        &oc.imageBuffer,
		"CFNumberGetValue":                                    &oc.numberGet,
	} {
		p, err := purego.Dlsym(purego.RTLD_DEFAULT, name)
		if err != nil {
			return fmt.Errorf("macvideo: %w", err)
		}
		purego.RegisterFunc(fn, p)
	}
	oc.consts = map[string]objc.ID{}
	for path, names := range objcConstants {
		for _, name := range names {
			p, err := purego.Dlsym(handles[path], name)
			if err != nil {
				return fmt.Errorf("macvideo: %w", err)
			}
			v := objc.ID(deref(p))
			if v == 0 {
				return fmt.Errorf("macvideo: constant %s is nil", name)
			}
			oc.consts[name] = v
		}
	}
	for cls := range selectors {
		name := strings.TrimPrefix(cls, "+")
		if objc.GetClass(name) == 0 {
			return fmt.Errorf("macvideo: no class %s (huddle needs macOS 14 or later)", name)
		}
	}
	return nil
}

// constant is a framework constant resolved by loadObjC.
func constant(name string) objc.ID {
	v, ok := oc.consts[name]
	if !ok {
		panic("macvideo: constant " + name + " isn't in the tables")
	}
	return v
}

func sel(name string) objc.SEL { return objc.RegisterName(name) }

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

// withPool runs fn on a locked OS thread inside an autorelease pool.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := oc.poolPush()
	defer oc.poolPop(pool)
	fn()
}

// nsString makes an autoreleased NSString (inside withPool).
func nsString(s string) objc.ID {
	return class("NSString").Send(sel("alloc")).Send(sel("initWithUTF8String:"), s).Send(sel("autorelease"))
}

// goString copies an NSString; nil is "".
func goString(s objc.ID) string {
	if s == 0 {
		return ""
	}
	p := objc.Send[*byte](s, sel("UTF8String"))
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// nsArray makes an autoreleased NSArray (inside withPool).
func nsArray(ids ...objc.ID) objc.ID {
	if len(ids) == 0 {
		return class("NSArray").Send(sel("array"))
	}
	return class("NSArray").Send(sel("arrayWithObjects:count:"), unsafe.Pointer(&ids[0]), uint(len(ids)))
}

// nsDict makes an autoreleased NSDictionary (inside withPool). A nil key
// or value would raise an exception, so it panics first.
func nsDict(kv ...objc.ID) objc.ID {
	n := len(kv) / 2
	keys, vals := make([]objc.ID, n), make([]objc.ID, n)
	for i := range n {
		keys[i], vals[i] = kv[2*i], kv[2*i+1]
		if keys[i] == 0 || vals[i] == 0 {
			panic(fmt.Sprintf("macvideo: nil in a dictionary at pair %d", i))
		}
	}
	return class("NSDictionary").Send(sel("dictionaryWithObjects:forKeys:count:"),
		unsafe.Pointer(&vals[0]), unsafe.Pointer(&keys[0]), uint(n))
}

// each calls fn with every element of an NSArray.
func each(arr objc.ID, fn func(objc.ID)) {
	if arr == 0 {
		return
	}
	n := objc.Send[uint](arr, sel("count"))
	for i := range n {
		fn(arr.Send(sel("objectAtIndex:"), i))
	}
}

// NSError is a framework call's NSError.
type NSError struct {
	What   string
	Domain string
	Code   int
	Desc   string
}

func (e *NSError) Error() string {
	return fmt.Sprintf("%s: %s (%s %d)", e.What, e.Desc, e.Domain, e.Code)
}

func nsError(what string, err objc.ID) error {
	if err == 0 {
		return fmt.Errorf("%s: no error given", what)
	}
	return &NSError{What: what, Domain: goString(err.Send(sel("domain"))), Code: objc.Send[int](err, sel("code")),
		Desc: goString(err.Send(sel("localizedDescription")))}
}

var errTimeout = errors.New("timed out")

// newQueue makes a serial dispatch queue that drains an autorelease pool
// after every work item. label must live as long as the process.
func newQueue(label []byte) (uintptr, error) {
	const autoreleaseWorkItem = 1 // DISPATCH_AUTORELEASE_FREQUENCY_WORK_ITEM
	q := oc.queueCreate(&label[0], oc.attrAutorelease(0, autoreleaseWorkItem))
	if q == 0 {
		return 0, errors.New("no dispatch queue")
	}
	return q, nil
}

// protocol is the runtime's protocol of that name. The frameworks declare
// their delegate protocols only in headers, so the runtime has them only
// once some loaded binary refers to one; when it doesn't, huddle
// registers them itself with the methods it implements (a selector and
// its type encoding each), so conformsToProtocol: answers truthfully.
// Adapted from reel's internal/mac/stream.go.
func protocol(name string, methods ...[2]string) *objc.Protocol {
	if p := objc.GetProtocol(name); p != nil {
		return p
	}
	p := objc.AllocateProtocol(name)
	if p == nil { // registered meanwhile
		return objc.GetProtocol(name)
	}
	for _, m := range methods {
		p.AddMethodDescription(sel(m[0]), m[1], false, true)
	}
	p.Register()
	return p
}

// waitHandler calls send with a completion block taking an NSError, and
// waits for it.
func waitHandler(what string, send func(done objc.Block)) error {
	got := make(chan error, 1)
	done := objc.NewBlock(func(_ objc.Block, nserr objc.ID) {
		if nserr != 0 {
			got <- nsError(what, nserr)
			return
		}
		got <- nil
	})
	defer done.Release()
	send(done)
	select {
	case err := <-got:
		return err
	case <-time.After(callTimeout):
		return fmt.Errorf("%s: %w", what, errTimeout)
	}
}

// cgRect is CoreGraphics' CGRect.
type cgRect struct{ X, Y, W, H float64 }

// Permission is the state of a privacy permission.
type Permission int

const (
	PermissionUnknown Permission = iota // not asked yet
	PermissionDenied                    // denied, or restricted by policy
	PermissionGranted
)

func (p Permission) String() string {
	switch p {
	case PermissionGranted:
		return "granted"
	case PermissionDenied:
		return "denied"
	}
	return "not asked"
}
