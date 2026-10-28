package main

// The camera and the share on macOS: capture (macvideo), a Sender per
// video (an encoder per layer) and its publication in the room. With
// -fake-camera both are the test pattern, so the buttons work without a
// camera or the picker (agents' runs, and an app run alone).
//
// setCamera and setShare block until the camera runs or the share has
// started (the bar calls them on goroutines of its own and shows them as
// busy meanwhile); what they return is short enough for the button.

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/platform/macvideo"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// What the buttons say when they can't.
var (
	errCamDenied    = errors.New("Allow in System Settings › Privacy & Security › Camera")
	errShareDenied  = errors.New("Allow in System Settings › Privacy & Security › Screen & System Audio Recording")
	errCamNotBundle = errors.New("Only Huddle.app can use the camera")
	errCamNoPicture = errors.New("The camera sent no picture")
	errCamFailed    = errors.New("The camera wouldn't start")
	errCamStopped   = errors.New("The camera stopped")
	errShareFailed  = errors.New("The share wouldn't start")
)

// localVideo is one video this side sends: its capture, its sender and
// its publication in the room.
type localVideo struct {
	kind rtc.VideoKind
	uid  string // the camera's (or "" for the default, or the pattern)
	send *macvideo.Sender
	stop func() // stops the capture

	mu  sync.Mutex
	pub rtc.VideoPub
}

// publish puts the video into room.
func (v *localVideo) publish(room rtc.Room) {
	pub, err := room.PublishVideo(v.send, v.kind)
	if err != nil {
		slog.Error("video: publish", "kind", v.kind, "err", err)
		return
	}
	v.mu.Lock()
	v.pub = pub
	v.mu.Unlock()
}

// unpublished forgets the publication (the room it was in is gone, and
// stopped it).
func (v *localVideo) unpublished() {
	v.mu.Lock()
	v.pub = nil
	v.mu.Unlock()
}

// close unpublishes and stops the capture.
func (v *localVideo) close() {
	v.mu.Lock()
	pub := v.pub
	v.pub = nil
	v.mu.Unlock()
	if pub != nil {
		pub.Stop()
	}
	v.send.Stop()
	if v.stop != nil {
		v.stop()
	}
}

// videoDecoder is the decoder for each remote video (VideoToolbox).
func videoDecoder() (rtc.VideoDecoder, error) { return macvideo.NewDecoder() }

// setCamera turns your camera on or off. On, it asks for the Camera
// permission if macOS hasn't yet, opens the camera picked in the device
// menu (devicePrefs.Camera) and returns once it runs; asked again while
// on, it moves to the camera picked if that changed.
func (c *call) setCamera(on bool) error {
	if c.noVideo {
		return errNoVideo
	}
	pick := c.devicePicks().Camera
	c.mu.Lock()
	cur := c.vid.cam
	if on && c.vid.camera && (c.fakeCamera || cur != nil && cur.uid == pick) {
		c.mu.Unlock()
		return nil
	}
	if !on && !c.vid.camera {
		c.mu.Unlock()
		return nil
	}
	c.vid.camGen++
	gen := c.vid.camGen
	c.vid.cam, c.vid.camErr = nil, ""
	c.vid.camera = on
	if on {
		c.vid.self = &video.Latest{}
	} else {
		c.vid.self = nil
	}
	c.mu.Unlock()
	if cur != nil {
		cur.close()
	}
	c.wakeUp()
	if !on {
		slog.Info("video: camera off")
		return nil
	}
	err := c.openCamera(gen, pick)
	if err != nil {
		c.mu.Lock()
		if c.vid.camGen == gen {
			c.vid.camera, c.vid.camErr, c.vid.self = false, err.Error(), nil
		}
		c.mu.Unlock()
		c.wakeUp()
	}
	return err
}

// openCamera opens the camera (or the pattern), waits for its first
// picture to learn its size, and publishes it.
func (c *call) openCamera(gen int, uid string) error {
	first := make(chan [2]int, 1)
	var send *macvideo.Sender
	var sendMu sync.Mutex
	frame := func(b *macvideo.Buffer, at time.Time) {
		defer b.Release()
		c.mu.Lock()
		self := c.vid.self
		current := c.vid.camGen == gen
		c.mu.Unlock()
		if !current {
			return
		}
		w, h := b.Size()
		select {
		case first <- [2]int{w, h}:
		default:
		}
		if self != nil {
			// The window mirrors your own tile; what's sent isn't.
			b.View(func(f video.Frame) {
				f.At, f.Mirror = at, true
				self.Put(f)
			})
			c.wakeUp()
		}
		sendMu.Lock()
		s := send
		sendMu.Unlock()
		if s != nil {
			s.Feed(b, at)
		}
	}
	var stop func()
	if c.fakeCamera {
		p, err := macvideo.StartPattern(macvideo.PatternConfig{W: 1280, H: 720, FPS: 30}, frame)
		if err != nil {
			slog.Error("video: the pattern camera", "err", err)
			return errCamFailed
		}
		stop = p.Stop
	} else {
		if !macvideo.InBundle() {
			return errCamNotBundle
		}
		switch macvideo.CameraAccess() {
		case macvideo.PermissionDenied:
			return errCamDenied
		case macvideo.PermissionUnknown:
			asked := make(chan bool, 1)
			if err := macvideo.RequestCameraAccess(func(ok bool) { asked <- ok }); err != nil {
				slog.Error("video: ask for the camera", "err", err)
				return errCamFailed
			}
			if !<-asked {
				return errCamDenied
			}
		}
		cs, err := macvideo.StartCamera(uid, frame, func(err error) { c.cameraStopped(gen, err) })
		switch {
		case errors.Is(err, macvideo.ErrNoCameraAccess):
			return errCamDenied
		case err != nil:
			slog.Error("video: open the camera", "err", err)
			return errCamFailed
		}
		stop = cs.Stop
	}
	var size [2]int
	select {
	case size = <-first:
	case <-time.After(5 * time.Second):
		stop()
		return errCamNoPicture
	}
	s, err := macvideo.NewSender(macvideo.SenderConfig{Layers: macvideo.CameraLayers(size[0], size[1]), FPS: 30})
	if err != nil {
		stop()
		slog.Error("video: camera sender", "err", err)
		return errCamFailed
	}
	lv := &localVideo{kind: rtc.Camera, uid: uid, send: s, stop: stop}
	c.mu.Lock()
	if c.vid.camGen != gen { // turned off meanwhile
		c.mu.Unlock()
		stop()
		return nil
	}
	c.vid.cam = lv
	room := c.room
	c.mu.Unlock()
	sendMu.Lock()
	send = s
	sendMu.Unlock()
	slog.Info("video: camera on", "w", size[0], "h", size[1], "fake", c.fakeCamera)
	if room != nil {
		lv.publish(room)
	}
	return nil
}

// cameraStopped is the camera stopping by itself (unplugged, taken).
func (c *call) cameraStopped(gen int, err error) {
	slog.Warn("video: the camera stopped", "err", err)
	c.mu.Lock()
	var old *localVideo
	if c.vid.camGen == gen {
		c.vid.camGen++
		c.vid.camera, c.vid.camErr, c.vid.self = false, errCamStopped.Error(), nil
		old, c.vid.cam = c.vid.cam, nil
	}
	c.mu.Unlock()
	if old != nil {
		old.close()
	}
	c.wakeUp()
}

// setShare starts sharing (the system picker chooses what) or stops.
// Starting returns once the share runs, or the picker was closed (nil:
// not an error).
func (c *call) setShare(on bool) error {
	if c.noVideo {
		return errNoVideo
	}
	c.mu.Lock()
	if on == c.vid.sharing {
		c.mu.Unlock()
		return nil
	}
	c.vid.shareGen++
	gen := c.vid.shareGen
	old := c.vid.share
	c.vid.share, c.vid.shareEr, c.vid.sharing = nil, "", false
	c.mu.Unlock()
	if old != nil {
		old.close()
	}
	c.wakeUp()
	if !on {
		slog.Info("video: share off")
		return nil
	}
	var send *macvideo.Sender
	var sendMu sync.Mutex
	frame := func(b *macvideo.Buffer, at time.Time) {
		defer b.Release()
		sendMu.Lock()
		s := send
		sendMu.Unlock()
		if s != nil {
			s.Feed(b, at)
		}
	}
	started := func(w, h int, stop func()) error {
		s, err := macvideo.NewSender(macvideo.SenderConfig{Layers: macvideo.ScreenLayers(w, h), FPS: macvideo.ShareFPS, Hold: true})
		if err != nil {
			stop()
			slog.Error("video: share sender", "err", err)
			return errShareFailed
		}
		lv := &localVideo{kind: rtc.Screen, send: s, stop: stop}
		c.mu.Lock()
		if c.vid.shareGen != gen {
			c.mu.Unlock()
			stop()
			return nil
		}
		c.vid.share, c.vid.sharing = lv, true
		room := c.room
		c.mu.Unlock()
		sendMu.Lock()
		send = s
		sendMu.Unlock()
		slog.Info("video: share on", "w", w, "h", h, "fake", c.fakeCamera)
		c.wakeUp()
		if room != nil {
			lv.publish(room)
		}
		return nil
	}
	var err error
	if c.fakeCamera {
		p, perr := macvideo.StartPattern(macvideo.PatternConfig{W: 1920, H: 1080, FPS: macvideo.ShareFPS}, frame)
		if perr != nil {
			slog.Error("video: the pattern share", "err", perr)
			err = errShareFailed
		} else {
			err = started(1920, 1080, p.Stop)
		}
	} else {
		type result struct {
			s   *macvideo.Share
			err error
		}
		got := make(chan result, 1)
		perr := macvideo.PickShare(macvideo.ShareRequest{
			Frame:   frame,
			Started: func(s *macvideo.Share, err error) { got <- result{s, err} },
			Stopped: func(err error) { c.shareStopped(gen, err) },
		})
		if perr != nil {
			slog.Error("video: the picker", "err", perr)
			err = errShareFailed
		} else {
			r := <-got
			switch {
			case errors.Is(r.err, macvideo.ErrCancelled):
				slog.Info("video: the picker was closed")
			case macvideo.ShareDenied(r.err):
				err = errShareDenied
			case r.err != nil:
				slog.Error("video: share", "err", r.err)
				err = errShareFailed
			default:
				w, h := r.s.Size()
				err = started(w, h, r.s.Stop)
			}
		}
	}
	if err != nil {
		c.mu.Lock()
		if c.vid.shareGen == gen {
			c.vid.shareEr = err.Error()
		}
		c.mu.Unlock()
		c.wakeUp()
	}
	return err
}

// shareStopped is the share ending by itself (the menu bar's stop, the
// window closed): not an error.
func (c *call) shareStopped(gen int, err error) {
	slog.Info("video: the share ended", "err", err)
	c.mu.Lock()
	var old *localVideo
	if c.vid.shareGen == gen {
		c.vid.shareGen++
		c.vid.sharing = false
		old, c.vid.share = c.vid.share, nil
	}
	c.mu.Unlock()
	if old != nil {
		old.close()
	}
	c.wakeUp()
}

// cameras lists the cameras without opening any or asking for anything.
func cameras() ([]cameraDevice, error) {
	cams, err := macvideo.Cameras()
	if err != nil {
		return nil, err
	}
	out := make([]cameraDevice, 0, len(cams))
	for _, cm := range cams {
		out = append(out, cameraDevice{UID: cm.UID, Name: cm.Name, Transport: cm.Kind.String(), Default: cm.Default})
	}
	return out, nil
}

// warmVideo loads the capture frameworks on a goroutine at start, so the
// device menu's first camera list (on the main thread) doesn't wait for
// them.
func warmVideo() { go macvideo.Cameras() }

// closeVideo stops the camera and the share (the app is closing).
func (c *call) closeVideo() {
	c.mu.Lock()
	vs := []*localVideo{c.vid.cam, c.vid.share}
	c.vid.cam, c.vid.share = nil, nil
	c.vid.camera, c.vid.sharing, c.vid.self = false, false, nil
	c.vid.camGen++
	c.vid.shareGen++
	c.mu.Unlock()
	for _, v := range vs {
		if v != nil {
			v.close()
		}
	}
}
