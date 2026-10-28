package main

import (
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/video"
)

// TestFakeCamera: with -fake-camera the Camera and Share calls run the
// pattern through the pipeline (no device, no picker, no room yet).
func TestFakeCamera(t *testing.T) {
	c := newCall(true, "", nil)
	c.fakeCamera = true
	if err := c.setCamera(true); err != nil {
		t.Fatal(err)
	}
	v := c.videoSnapshot()
	if !v.camera || v.cam == nil || v.self == nil {
		t.Fatalf("camera %v, pipeline %v, self-view %v", v.camera, v.cam != nil, v.self != nil)
	}
	var got video.Frame
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := v.self.Take(0, func(f video.Frame) { got.CopyFrom(f) }); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no self-view frame")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.W != 1280 || got.H != 720 || !got.Mirror {
		t.Fatalf("self-view %dx%d mirror %v", got.W, got.H, got.Mirror)
	}
	if _, ok := video.ReadCode(got); !ok {
		t.Fatal("the self-view isn't the pattern")
	}
	if err := c.setCamera(true); err != nil || c.videoSnapshot().cam != v.cam {
		t.Fatalf("asked again: %v, a new pipeline %v", err, c.videoSnapshot().cam != v.cam)
	}
	if err := c.setShare(true); err != nil {
		t.Fatal(err)
	}
	if s := c.videoSnapshot(); !s.sharing || s.share == nil {
		t.Fatal("not sharing")
	}
	c.setShare(false)
	c.setCamera(false)
	if s := c.videoSnapshot(); s.camera || s.sharing || s.self != nil || s.cam != nil || s.share != nil {
		t.Fatalf("still on: %+v", s)
	}
	c.noVideo = true
	if err := c.setCamera(true); err != errNoVideo {
		t.Fatalf("with -no-video: %v", err)
	}
}

// TestCamerasQuick: the device menu lists cameras on the main thread, so
// listing (after the frameworks are loaded) must be quick; it opens
// nothing.
func TestCamerasQuick(t *testing.T) {
	start := time.Now()
	if _, err := cameras(); err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	start = time.Now()
	cams, err := cameras()
	if err != nil {
		t.Fatal(err)
	}
	again := time.Since(start)
	t.Logf("%d cameras: first list %v (loads the frameworks), then %v", len(cams), first.Round(time.Millisecond), again.Round(time.Millisecond))
	if again > 50*time.Millisecond {
		t.Fatalf("listing took %v", again)
	}
}
