package main

// The call's video, between the pipeline and the window. Stage 2 builds it
// in two tracks that meet here: Track P (capture, encoding, the room's
// video, this file's bodies) and Track U (the tiles and the stage, which
// read videoState and call setCamera, setShare and cameras). Until Track
// P lands, the camera and the share say they aren't built yet.

import (
	"errors"
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
}

// cameraDevice is a camera the device menu lists.
type cameraDevice struct {
	UID, Name string
	Transport string // built-in, USB, iPhone (Continuity)
	Default   bool
}

var errVideoNotYet = errors.New("video isn't built yet")

// videoSnapshot is the video state for one frame.
func (c *call) videoSnapshot() videoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.vid
	v.tracks = slices.Clone(v.tracks)
	return v
}

// setCamera turns your camera on or off.
func (c *call) setCamera(on bool) error { return errVideoNotYet }

// setShare starts sharing (the system picker chooses what) or stops.
func (c *call) setShare(on bool) error { return errVideoNotYet }

// cameras lists the cameras without opening any.
func cameras() ([]cameraDevice, error) { return nil, errVideoNotYet }
