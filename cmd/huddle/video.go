package main

// The call's video, between the pipeline and the window. Stage 2 builds it
// in two tracks that meet here: Track P (capture, encoding, the room's
// video: this file and video_darwin.go) and Track U (the tiles and the
// stage, which read videoState and call setCamera, setShare and cameras).
//
// The camera and the share are opened on their own goroutines (opening a
// camera takes about a second); what they show is published into the
// room the call is in, and again into the next one when voice reconnects.
// Remote cameras and screens arrive through the room's events into
// videoState.tracks.

import (
	"errors"
	"log/slog"
	"slices"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// videoState is what the window shows of the call's video.
type videoState struct {
	tracks  []rtc.VideoTrack // remote cameras and screens, as they arrived
	self    *video.Latest    // your camera's picture while it's on (nil when off)
	camera  bool             // your camera is on
	sharing bool             // you're sharing a screen or window
	camErr  string           // why the camera isn't on (no permission, none plugged in)
	shareEr string           // why the share stopped or didn't start

	// The pipeline behind them (video_darwin.go), not for the window.
	cam, share *localVideo
	camGen     int // counts setCamera calls: an open that was turned off meanwhile closes itself
	shareGen   int
	roomGen    int // counts rooms: a room's late events after hang-up are dropped
}

// cameraDevice is a camera the device menu lists.
type cameraDevice struct {
	UID, Name string
	Transport string // built-in, USB, iPhone (Continuity)
	Default   bool
}

var errNoVideo = errors.New("video is off (-no-video)")

// videoSnapshot is the video state for one frame.
func (c *call) videoSnapshot() videoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vid
	v.tracks = slices.Clone(v.tracks)
	return v
}

// videoEvents are the room's video events for the call in room number gen.
func (c *call) videoEvents(ev *rtc.Events, gen int) {
	ev.VideoAdded = func(t rtc.VideoTrack) {
		c.mu.Lock()
		if c.vid.roomGen == gen {
			c.vid.tracks = append(c.vid.tracks, t)
		}
		c.mu.Unlock()
		slog.Info("video: track added", "identity", t.Identity(), "kind", t.Kind())
		c.wakeUp()
	}
	ev.VideoRemoved = func(t rtc.VideoTrack) {
		c.mu.Lock()
		c.vid.tracks = slices.DeleteFunc(c.vid.tracks, func(u rtc.VideoTrack) bool { return u == t })
		c.mu.Unlock()
		slog.Info("video: track removed", "identity", t.Identity(), "kind", t.Kind())
		c.wakeUp()
	}
}

// publishVideo puts the camera and the share, if on, into a room just
// joined.
func (c *call) publishVideo(room rtc.Room) {
	c.mu.Lock()
	vs := []*localVideo{c.vid.cam, c.vid.share}
	c.mu.Unlock()
	for _, v := range vs {
		if v != nil {
			v.publish(room)
		}
	}
}

// leftVideo is the call hanging up: the room's tracks and publications
// went with it.
func (c *call) leftVideo() {
	c.mu.Lock()
	c.vid.roomGen++
	c.vid.tracks = nil
	vs := []*localVideo{c.vid.cam, c.vid.share}
	c.mu.Unlock()
	for _, v := range vs {
		if v != nil {
			v.unpublished()
		}
	}
	c.wakeUp()
}

func (c *call) wakeUp() {
	if c.wake != nil {
		c.wake()
	}
}
