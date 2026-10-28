package macvideo

// Screen sharing: what to share is picked in the system's own picker
// (SCContentSharingPicker, macOS 14 and later; the app never lists other
// apps' windows), and captured with ScreenCaptureKit as NV12 (420v, BT.709)
// at up to 15 fps and 1920 wide. A still screen sends few frames:
// ScreenCaptureKit delivers one only when something changed.
//
// Adapted from reel's internal/mac/stream.go.

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// Screen capture's limits.
const (
	ShareMaxWidth = 1920
	ShareFPS      = 15
	// shareQueueDepth is the surfaces ScreenCaptureKit keeps: a frame
	// held past the next one or two takes one away from it.
	shareQueueDepth = 6
)

// SCContentSharingPickerMode (SCContentSharingPicker.h): a window or a
// display.
const (
	pickerSingleWindow  = 1 << 0
	pickerSingleDisplay = 1 << 4
)

// SCStreamOutputType and SCFrameStatus (SCStream.h).
const (
	outputScreen   = 0
	frameComplete  = 0
	cfNumberSInt32 = 3
)

// ErrCancelled is a share the person closed the picker on.
var ErrCancelled = errors.New("share: cancelled")

// ShareRequest is what PickShare reports to.
type ShareRequest struct {
	// Frame gets each picture of the share.
	Frame FrameFunc
	// Started hears once: the share started (with its size), or why not
	// (ErrCancelled when the picker was closed).
	Started func(s *Share, err error)
	// Stopped hears once if the share stops by itself (the menu bar's
	// stop, the window closed), with why.
	Stopped func(err error)
}

// Share is a running screen or window share.
type Share struct {
	stream, output objc.ID
	req            ShareRequest
	w, h           int

	mu      sync.Mutex
	stopped bool
}

// Size is the share's picture size.
func (s *Share) Size() (w, h int) { return s.w, s.h }

var shares = struct {
	sync.Mutex
	m       map[objc.ID]*Share // by output object
	pending *ShareRequest      // the request the picker is up for
	live    int
}{m: map[objc.ID]*Share{}}

var screen struct {
	once     sync.Once
	err      error
	output   objc.Class
	observer objc.ID
	queue    uintptr
}

var screenQueueLabel = []byte("com.anthonyrego.huddle.screen\x00")

// loadScreen registers the stream's output class and the picker's
// observer class, makes the frames' queue and the one observer object,
// once.
func loadScreen() error {
	screen.once.Do(func() {
		if screen.err = loadObjC(); screen.err != nil {
			return
		}
		if screen.queue, screen.err = newQueue(screenQueueLabel); screen.err != nil {
			return
		}
		outProtos := []*objc.Protocol{
			protocol("SCStreamOutput", [2]string{"stream:didOutputSampleBuffer:ofType:", "v@:@^vq"}),
			protocol("SCStreamDelegate", [2]string{"stream:didStopWithError:", "v@:@@"}),
		}
		if screen.output, screen.err = objc.RegisterClass("HuddleStreamOutput", objc.GetClass("NSObject"), outProtos, nil, []objc.MethodDef{
			{Cmd: sel("stream:didOutputSampleBuffer:ofType:"), Fn: shareDidOutput},
			{Cmd: sel("stream:didStopWithError:"), Fn: shareDidStop},
		}); screen.err != nil {
			return
		}
		pickProtos := []*objc.Protocol{protocol("SCContentSharingPickerObserver",
			[2]string{"contentSharingPicker:didCancelForStream:", "v@:@@"},
			[2]string{"contentSharingPicker:didUpdateWithFilter:forStream:", "v@:@@@"},
			[2]string{"contentSharingPickerStartDidFailWithError:", "v@:@"})}
		var obs objc.Class
		if obs, screen.err = objc.RegisterClass("HuddlePickerObserver", objc.GetClass("NSObject"), pickProtos, nil, []objc.MethodDef{
			{Cmd: sel("contentSharingPicker:didCancelForStream:"), Fn: pickerCancelled},
			{Cmd: sel("contentSharingPicker:didUpdateWithFilter:forStream:"), Fn: pickerPicked},
			{Cmd: sel("contentSharingPickerStartDidFailWithError:"), Fn: pickerFailed},
			{Cmd: sel("presentPicker:"), Fn: presentPicker},
		}); screen.err != nil {
			return
		}
		withPool(func() { screen.observer = objc.ID(obs).Send(sel("alloc")).Send(sel("init")) })
	})
	return screen.err
}

// PickShare shows the system picker (on the main thread, from any
// goroutine: the app's run loop runs it); what's picked starts a share,
// which req.Started hears about. One pick at a time.
func PickShare(req ShareRequest) error {
	if err := loadScreen(); err != nil {
		return fmt.Errorf("share: %w", err)
	}
	shares.Lock()
	if shares.pending != nil {
		shares.Unlock()
		return errors.New("share: the picker is already up")
	}
	shares.pending = &req
	shares.Unlock()
	withPool(func() {
		screen.observer.Send(sel("performSelectorOnMainThread:withObject:waitUntilDone:"), sel("presentPicker:"), objc.ID(0), false)
	})
	return nil
}

// presentPicker is the observer's presentPicker:, on the main thread.
func presentPicker(_ objc.ID, _ objc.SEL, _ objc.ID) {
	withPool(func() {
		picker := class("SCContentSharingPicker").Send(sel("sharedPicker"))
		conf := class("SCContentSharingPickerConfiguration").Send(sel("alloc")).Send(sel("init")).Send(sel("autorelease"))
		conf.Send(sel("setAllowedPickerModes:"), uint(pickerSingleWindow|pickerSingleDisplay))
		picker.Send(sel("setDefaultConfiguration:"), conf)
		picker.Send(sel("addObserver:"), screen.observer) // a set: adding again is harmless
		picker.Send(sel("setActive:"), true)
		picker.Send(sel("present"))
	})
}

// takePending is the request the picker answered (nil if none).
func takePending() *ShareRequest {
	shares.Lock()
	defer shares.Unlock()
	r := shares.pending
	shares.pending = nil
	return r
}

// pickerPicked is contentSharingPicker:didUpdateWithFilter:forStream:.
func pickerPicked(_ objc.ID, _ objc.SEL, _ objc.ID, filter, stream objc.ID) {
	if stream != 0 {
		// A running share's content changed in the picker: same stream,
		// new filter (ScreenCaptureKit scales it to the same size).
		filter.Send(sel("retain"))
		go func() {
			withPool(func() {
				err := waitHandler("share: update", func(done objc.Block) {
					stream.Send(sel("updateContentFilter:completionHandler:"), filter, done)
				})
				if err != nil {
					slog.Warn("share: content not changed", "err", err)
				}
				filter.Send(sel("release"))
			})
		}()
		return
	}
	req := takePending()
	if req == nil {
		return
	}
	filter.Send(sel("retain"))
	// Off the picker's thread: starting waits on a completion handler.
	go func() {
		s, err := startShare(filter, *req)
		withPool(func() { filter.Send(sel("release")) })
		if err != nil {
			pickerInactive()
		}
		req.Started(s, err)
	}()
}

// pickerCancelled is contentSharingPicker:didCancelForStream:.
func pickerCancelled(_ objc.ID, _ objc.SEL, _ objc.ID, stream objc.ID) {
	if stream != 0 {
		return
	}
	if req := takePending(); req != nil {
		go func() {
			pickerInactive()
			req.Started(nil, ErrCancelled)
		}()
	}
}

// pickerFailed is contentSharingPickerStartDidFailWithError:.
func pickerFailed(_ objc.ID, _ objc.SEL, nserr objc.ID) {
	err := nsError("share: the picker failed", nserr)
	if req := takePending(); req != nil {
		go func() {
			pickerInactive()
			req.Started(nil, err)
		}()
	}
}

// pickerInactive takes the picker out of the menu bar when nothing is
// shared.
func pickerInactive() {
	shares.Lock()
	idle := shares.live == 0 && shares.pending == nil
	shares.Unlock()
	if idle {
		withPool(func() { class("SCContentSharingPicker").Send(sel("sharedPicker")).Send(sel("setActive:"), false) })
	}
}

// shareSize is the size a filter's content is captured at: its own pixels,
// at most ShareMaxWidth wide, even.
func shareSize(r cgRect, scale float64) (w, h int) {
	if scale <= 0 {
		scale = 1
	}
	pw, ph := r.W*scale, r.H*scale
	if pw > ShareMaxWidth {
		ph = ph * ShareMaxWidth / pw
		pw = ShareMaxWidth
	}
	w, h = int(math.Round(pw))&^1, int(math.Round(ph))&^1
	return max(w, 64), max(h, 64)
}

// startShare starts a stream of what the filter shows.
func startShare(filter objc.ID, req ShareRequest) (*Share, error) {
	s := &Share{req: req}
	var err error
	withPool(func() {
		r := objc.Send[cgRect](filter, sel("contentRect"))
		scale := float64(objc.Send[float32](filter, sel("pointPixelScale")))
		s.w, s.h = shareSize(r, scale)
		conf := class("SCStreamConfiguration").Send(sel("alloc")).Send(sel("init")).Send(sel("autorelease"))
		conf.Send(sel("setWidth:"), uint(s.w))
		conf.Send(sel("setHeight:"), uint(s.h))
		conf.Send(sel("setMinimumFrameInterval:"), cmTime{Value: 1, Timescale: ShareFPS, Flags: cmTimeValid})
		conf.Send(sel("setPixelFormat:"), formatNV12)
		conf.Send(sel("setQueueDepth:"), shareQueueDepth)
		conf.Send(sel("setShowsCursor:"), true)
		conf.Send(sel("setColorSpaceName:"), constant("kCGColorSpaceSRGB"))
		conf.Send(sel("setColorMatrix:"), constant("kCGDisplayStreamYCbCrMatrix_ITU_R_709_2"))

		s.output = objc.ID(screen.output).Send(sel("alloc")).Send(sel("init"))
		shares.Lock()
		shares.m[s.output] = s
		shares.Unlock()
		s.stream = class("SCStream").Send(sel("alloc")).Send(sel("initWithFilter:configuration:delegate:"), filter, conf, s.output)
		if s.stream == 0 {
			err = errors.New("share: ScreenCaptureKit wouldn't make a stream")
			return
		}
		var nserr objc.ID
		if !objc.Send[bool](s.stream, sel("addStreamOutput:type:sampleHandlerQueue:error:"), s.output, outputScreen, screen.queue, unsafe.Pointer(&nserr)) {
			err = nsError("share: add the output", nserr)
			return
		}
		err = waitHandler("share: start", func(done objc.Block) {
			s.stream.Send(sel("startCaptureWithCompletionHandler:"), done)
		})
	})
	if err != nil {
		s.forget()
		return nil, err
	}
	shares.Lock()
	shares.live++
	shares.Unlock()
	slog.Info("share: started", "w", s.w, "h", s.h)
	return s, nil
}

// shareDidOutput is stream:didOutputSampleBuffer:ofType:, on the share's
// queue. Only complete frames carry a picture; C calls only.
func shareDidOutput(self objc.ID, _ objc.SEL, _ objc.ID, sbuf uintptr, kind int) {
	if kind != outputScreen || sbuf == 0 {
		return
	}
	shares.Lock()
	s := shares.m[self]
	shares.Unlock()
	if s == nil || frameStatus(sbuf) != frameComplete {
		return
	}
	img := oc.imageBuffer(sbuf)
	if img == 0 {
		return
	}
	s.req.Frame(retainBuffer(img), time.Now())
}

// frameStatus is a sample's SCStreamFrameInfoStatus (-1 if it has none).
func frameStatus(sbuf uintptr) int {
	arr := cm.attachments(sbuf, false)
	if arr == 0 || cf.arrayCount(arr) == 0 {
		return -1
	}
	v := cf.dictGet(cf.arrayAt(arr, 0), uintptr(constant("SCStreamFrameInfoStatus")))
	if v == 0 {
		return -1
	}
	var st int32
	if !oc.numberGet(v, cfNumberSInt32, unsafe.Pointer(&st)) {
		return -1
	}
	return int(st)
}

// shareDidStop is stream:didStopWithError:: the share stopped by itself.
func shareDidStop(self objc.ID, _ objc.SEL, _ objc.ID, nserr objc.ID) {
	shares.Lock()
	s := shares.m[self]
	shares.Unlock()
	if s == nil {
		return
	}
	err := nsError("share: stopped", nserr)
	slog.Warn("share: stopped by itself", "err", err)
	go s.finish(err)
}

// Stop stops the share and waits for ScreenCaptureKit to say so.
func (s *Share) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.req.Stopped = nil // asked for: not news
	s.mu.Unlock()
	withPool(func() {
		if err := waitHandler("share: stop", func(done objc.Block) {
			s.stream.Send(sel("stopCaptureWithCompletionHandler:"), done)
		}); err != nil {
			slog.Warn("share: stop", "err", err)
		}
	})
	s.finish(nil)
}

// finish lets the share go and tells Stopped once.
func (s *Share) finish(err error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	stopped := s.req.Stopped
	s.mu.Unlock()
	s.forget()
	shares.Lock()
	shares.live--
	shares.Unlock()
	pickerInactive()
	if stopped != nil {
		stopped(err)
	}
}

// forget drops the share from the output map. The SCStream and its
// output object are leaked on purpose: the stream may still be finishing
// on its queues, and it's a few objects per share.
func (s *Share) forget() {
	if s.output != 0 {
		shares.Lock()
		delete(shares.m, s.output)
		shares.Unlock()
	}
}

// scStreamUserDeclined is SCStreamErrorUserDeclined (SCError.h): the
// Screen Recording permission was refused.
const scStreamUserDeclined = -3801

// ShareDenied reports whether a share failed for want of the Screen
// Recording permission.
func ShareDenied(err error) bool {
	var ns *NSError
	return errors.As(err, &ns) && ns.Code == scStreamUserDeclined
}
