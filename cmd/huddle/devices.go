package main

// The call's devices: which mic and speakers, and whether the mic goes
// through echo cancelling (macOS voice processing). The device menu picks
// them; the pick takes effect at once and is kept in devices.json for the
// next run. A device picked but not plugged in is skipped for the
// system's default until it's back.
//
// The room publishes one mic and plays into one speaker for the whole call
// (liveMic, liveSpeaker): they read from and write to whichever devices
// are open now, so switching devices never touches the room. Between
// devices, and with no mic, the mic gives silence on time, so the track's
// clock never stops.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anthonyrego/huddle/internal/platform/audio"
	"github.com/anthonyrego/huddle/internal/rtc"
)

// devicePrefs is what the device menu picked.
type devicePrefs struct {
	Mic      string `json:"mic,omitempty"`      // a device UID; "" follows the system's default
	Speakers string `json:"speakers,omitempty"` // the same
	EchoOff  bool   `json:"echoOff,omitempty"`  // no echo cancelling (headphones)
	Camera   string `json:"camera,omitempty"`   // a camera's UID (AVCaptureDevice uniqueID); "" the default
}

func devicesPath(data string) string { return filepath.Join(data, "devices.json") }

func readDevicePrefs(data string) devicePrefs {
	var p devicePrefs
	if b, err := os.ReadFile(devicesPath(data)); err == nil {
		if err := json.Unmarshal(b, &p); err != nil {
			slog.Warn("devices: unreadable choice; using the defaults", "err", err)
			return devicePrefs{}
		}
	}
	return p
}

func writeDevicePrefs(data string, p devicePrefs) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := devicesPath(data) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, devicesPath(data))
}

// devState is how opening the devices went, for the bar and the menu.
type devState struct {
	echoErr string // why echo cancelling couldn't start ("" when it did, or wasn't asked for)
	micErr  string // why the mic couldn't open
}

// openDevices opens the voice path on the devices picked, falling back a
// step at a time: without echo cancelling when voice processing won't
// start on them, to the speakers alone when the mic won't open, and to the
// system's speakers when the ones picked won't.
func (c *call) openDevices(p devicePrefs) (*audio.Voice, devState, error) {
	var ds devState
	cfg := audio.VoiceConfig{SpeakerOnly: c.noVoice, Mic: p.Mic, Speakers: p.Speakers, NoEchoCancel: p.EchoOff}
	if c.noVoice {
		cfg.Mic, cfg.NoEchoCancel = "", false
	}
	if cfg.Mic != "" || cfg.Speakers != "" {
		devs, err := audio.VoiceDevices()
		if err != nil {
			slog.Warn("devices: can't list them; using the defaults", "err", err)
			cfg.Mic, cfg.Speakers = "", ""
		}
		if cfg.Mic != "" && !hasDevice(devs, cfg.Mic, true) {
			slog.Warn("devices: the mic picked isn't plugged in; using the default")
			cfg.Mic = ""
		}
		if cfg.Speakers != "" && !hasDevice(devs, cfg.Speakers, false) {
			slog.Warn("devices: the speakers picked aren't plugged in; using the default")
			cfg.Speakers = ""
		}
	}
	v, err := audio.OpenVoice(cfg)
	if err != nil && !cfg.SpeakerOnly && !cfg.NoEchoCancel {
		slog.Error("voice: echo cancelling won't start on these devices; the mic without it", "err", err)
		ds.echoErr = err.Error()
		cfg.NoEchoCancel = true
		v, err = audio.OpenVoice(cfg)
	}
	if err != nil && !cfg.SpeakerOnly {
		slog.Error("voice: the mic won't open; listening only", "err", err)
		ds.micErr = err.Error()
		cfg.SpeakerOnly = true
		v, err = audio.OpenVoice(cfg)
	}
	if err != nil && cfg.Speakers != "" {
		slog.Error("voice: the speakers picked won't open; the system's", "err", err)
		cfg.Speakers = ""
		v, err = audio.OpenVoice(cfg)
	}
	return v, ds, err
}

func hasDevice(devs []audio.VoiceDevice, uid string, input bool) bool {
	for _, d := range devs {
		if d.UID == uid && (input && d.In > 0 || !input && d.Out > 0) {
			return true
		}
	}
	return false
}

// pick switches to the devices in p and keeps them for the next run. The
// switch runs on its own goroutine: opening takes up to a few seconds.
func (c *call) pick(p devicePrefs) {
	c.mu.Lock()
	c.prefs = p
	room := c.room
	c.mu.Unlock()
	if c.data != "" {
		if err := writeDevicePrefs(c.data, p); err != nil {
			slog.Warn("devices: couldn't keep the choice", "err", err)
		}
	}
	slog.Info("devices: picked", "mic", p.Mic != "", "speakers", p.Speakers != "", "echoOff", p.EchoOff)
	if room == nil {
		return // the next join opens them
	}
	go func() {
		c.switchMu.Lock()
		defer c.switchMu.Unlock()
		c.mu.Lock()
		old := c.dev
		c.dev = nil
		same := c.room == room
		c.mu.Unlock()
		if !same {
			return
		}
		c.update(func(s *callState) bool { s.status, s.canMute = callSwitching, false; return true })
		if old != nil {
			old.Close()
		}
		dev, ds, err := c.openDevices(p)
		if err != nil {
			slog.Error("voice: no devices after the switch", "err", err)
			c.update(func(s *callState) bool { s.status, s.detail = callLost, "speakers: "+err.Error(); return true })
			return
		}
		c.setDevice(room, dev, ds)
	}()
}

// setDevice makes dev the call's devices if the call is still in room,
// and shows what they can do.
func (c *call) setDevice(room rtc.Room, dev *audio.Voice, ds devState) {
	c.mu.Lock()
	if c.room != room {
		c.mu.Unlock()
		dev.Close()
		return
	}
	c.dev = dev
	c.mu.Unlock()
	info := dev.Info()
	c.update(func(s *callState) bool {
		s.canMute = !info.SpeakerOnly
		s.noEcho = !info.SpeakerOnly && !info.EchoCancel && ds.echoErr != ""
		s.echoErr, s.detail = ds.echoErr, ds.micErr
		switch {
		case !s.canMute:
			s.status = callListening
		case s.muted:
			s.status = callMuted
		default:
			s.status = callOn
		}
		return true
	})
}

// current is the open devices (nil between them).
func (c *call) current() *audio.Voice {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dev
}

// frameTime is how long a 48 kHz frame of n samples lasts.
func frameTime(n int) time.Duration { return time.Duration(n) * time.Second / 48000 }

// liveMic is the mic the room publishes (rtc.Mic).
type liveMic struct {
	c    *call
	next time.Time // when the next silent frame is due
}

func (m *liveMic) Read(ctx context.Context, frame []float32) error {
	for {
		dev := m.c.current()
		if dev != nil {
			err := dev.Mic().Read(ctx, frame)
			if err == nil {
				m.next = time.Time{}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if m.c.current() != dev {
				continue // switched while reading
			}
		}
		return pace(ctx, &m.next, frame, true)
	}
}

// liveSpeaker is the speaker the room plays into (rtc.Speaker).
type liveSpeaker struct {
	c    *call
	next time.Time
}

func (s *liveSpeaker) Write(ctx context.Context, frame []float32) error {
	dev := s.c.current()
	if dev != nil {
		err := dev.Speaker().Write(ctx, frame)
		if err == nil || ctx.Err() != nil {
			s.next = time.Time{}
			return err
		}
	}
	return pace(ctx, &s.next, frame, false)
}

// pace waits for the frame's turn on a clock of its own, with no device:
// the mic's silence, or the speakers' audio let go. It clears the frame
// for the mic.
func pace(ctx context.Context, next *time.Time, frame []float32, silence bool) error {
	now := time.Now()
	if next.IsZero() || next.Before(now.Add(-100*time.Millisecond)) {
		*next = now
	}
	if wait := next.Sub(now); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	*next = next.Add(frameTime(len(frame)))
	if silence {
		clear(frame)
	}
	return nil
}

// Latency is how long after a frame is written it's heard: what the
// player has queued plus the output's own delay (the room holds video
// frames against it, for lip sync).
func (s *liveSpeaker) Latency() time.Duration {
	dev := s.c.current()
	if dev == nil {
		return 0
	}
	i := dev.Info()
	return i.Latency + i.Queued
}
