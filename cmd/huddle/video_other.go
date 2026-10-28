//go:build !darwin

package main

// Video needs VideoToolbox, AVFoundation and ScreenCaptureKit: elsewhere
// the buttons say so, remote videos aren't decoded, and nothing is sent.

import (
	"errors"

	"github.com/anthonyrego/huddle/internal/rtc"
)

var errVideoMacOnly = errors.New("video needs macOS")

type localVideo struct{}

func (*localVideo) publish(rtc.Room) {}
func (*localVideo) unpublished()     {}

func videoDecoder() (rtc.VideoDecoder, error) { return nil, errVideoMacOnly }

func (c *call) setCamera(on bool) error {
	if !on {
		return nil
	}
	return errVideoMacOnly
}

func (c *call) setShare(on bool) error {
	if !on {
		return nil
	}
	return errVideoMacOnly
}

func (c *call) closeVideo() {}

func warmVideo() {}

func cameras() ([]cameraDevice, error) { return nil, errVideoMacOnly }
