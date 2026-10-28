package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/anthonyrego/huddle/internal/platform/macvideo"
	"github.com/anthonyrego/huddle/internal/rtc"
	rtcroom "github.com/anthonyrego/huddle/internal/rtc/room"
)

// runCam publishes the test pattern as a camera or a screen until ctx
// ends. It subscribes to nothing.
func runCam(ctx context.Context, join rtc.Join, share bool) error {
	room, err := (&rtcroom.Connector{SendOnly: true}).Connect(ctx, join, rtc.Events{})
	if err != nil {
		return err
	}
	defer room.Leave()
	kind, w, h, fps := rtc.Camera, 1280, 720, 30
	layers := macvideo.CameraLayers(w, h)
	if share {
		kind, w, h, fps = rtc.Screen, 1920, 1080, macvideo.ShareFPS
		layers = macvideo.ScreenLayers(w, h)
	}
	s, err := macvideo.NewSender(macvideo.SenderConfig{Layers: layers, FPS: fps, Hold: share})
	if err != nil {
		return err
	}
	p, err := macvideo.StartPattern(macvideo.PatternConfig{W: w, H: h, FPS: fps, FlashEvery: fps}, func(b *macvideo.Buffer, at time.Time) {
		s.Feed(b, at)
		b.Release()
	})
	if err != nil {
		return err
	}
	defer p.Stop()
	if _, err := room.PublishVideo(s, kind); err != nil {
		return err
	}
	slog.Info("bot: publishing the pattern", "kind", kind, "w", w, "h", h, "fps", fps)
	<-ctx.Done()
	return nil
}
