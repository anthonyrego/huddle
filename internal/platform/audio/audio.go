// Package audio opens the microphone and the speakers as SDL3 audio streams
// in the formats Live uses. Both are polled rather than driven by callbacks:
// SDL locks its streams internally, so any goroutine may call these methods.
// Call platform.InitAudio (or platform.Init plus the audio subsystem) first.
//
// Copied from an earlier project of the author's.
package audio

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/pcm"
)

const (
	InRate  = 16000 // what Live takes from the microphone
	OutRate = 24000 // what Live sends back
)

// Mic is a microphone giving mono s16 at InRate, polled without blocking.
// Capture is one; the voice processing engine has another.
type Mic interface {
	Available() int
	Read(buf []byte) (int, error)
	Close()
}

// Speaker plays mono s16 at OutRate and keeps the stream positions the
// voice loop times things by, in bytes at OutRate: Written is everything put
// and not cleared, Queued what is still waiting to be played, Played what
// the listener has heard. Playback is one; the voice processing engine has
// another.
type Speaker interface {
	Put(b []byte) error
	Written() int64
	Queued() int64
	Played() int64
	Latency() time.Duration
	Flush() error
	Clear() int64
	Close()
}

// Capture is the default microphone, converted by SDL to mono s16 at InRate.
type Capture struct {
	s *sdl.AudioStream
}

// OpenCapture opens the default recording device. On macOS the first call
// asks for microphone access for the terminal (or app) running the process.
func OpenCapture() (*Capture, error) {
	spec := &sdl.AudioSpec{Format: sdl.AUDIO_S16LE, Channels: 1, Freq: InRate}
	s := sdl.AUDIO_DEVICE_DEFAULT_RECORDING.OpenAudioDeviceStream(spec, 0)
	if s == nil {
		return nil, errors.New("SDL could not open the microphone (on macOS, check System Settings › Privacy & Security › Microphone)")
	}
	if err := s.ResumeDevice(); err != nil {
		s.Destroy()
		return nil, err
	}
	return &Capture{s: s}, nil
}

// Available is how many captured bytes are ready to read.
func (c *Capture) Available() int {
	n, err := c.s.Available()
	if err != nil {
		return 0
	}
	return int(n)
}

// Read fills buf with captured audio without blocking and returns how many
// bytes it wrote (0 when nothing is ready).
func (c *Capture) Read(buf []byte) (int, error) {
	n, err := c.s.Data(buf)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Close stops capturing.
func (c *Capture) Close() { c.s.Destroy() }

// Playback is the default output device, fed mono s16 at OutRate. It counts
// what it has been given so callers can tell how much has been heard.
type Playback struct {
	s       *sdl.AudioStream
	written atomic.Int64 // bytes put and not discarded by Clear
	latency int64        // the device's own buffer, in bytes at OutRate
}

// OpenPlayback opens the default playback device.
func OpenPlayback() (*Playback, error) {
	spec := &sdl.AudioSpec{Format: sdl.AUDIO_S16LE, Channels: 1, Freq: OutRate}
	s := sdl.AUDIO_DEVICE_DEFAULT_PLAYBACK.OpenAudioDeviceStream(spec, 0)
	if s == nil {
		return nil, errors.New("SDL could not open the speakers")
	}
	p := &Playback{s: s}
	if dev, frames, err := s.Device().Format(); err == nil && dev.Freq > 0 {
		p.latency = int64(frames) * OutRate / int64(dev.Freq) * pcm.BytesPerSample
	}
	if err := s.ResumeDevice(); err != nil {
		s.Destroy()
		return nil, err
	}
	return p, nil
}

// Put queues pcm to play after everything already queued.
func (p *Playback) Put(b []byte) error {
	if err := p.s.PutData(b); err != nil {
		return err
	}
	p.written.Add(int64(len(b)))
	return nil
}

// Written is the stream position after everything put so far, in bytes. A
// position taken now is reached by Played once that audio is heard.
func (p *Playback) Written() int64 { return p.written.Load() }

// Queued is how many bytes are waiting in the stream, not yet handed to the
// device.
func (p *Playback) Queued() int64 {
	q, err := p.s.Queued()
	if err != nil {
		return 0
	}
	return int64(q)
}

// Played is the stream position the listener has heard up to: what was put,
// less what is still queued, less the device's buffer. Output latency SDL
// can't see (Bluetooth headphones add 100–250 ms) is not included.
func (p *Playback) Played() int64 {
	return max(p.written.Load()-p.Queued()-p.latency, 0)
}

// Latency is the device buffer as a duration.
func (p *Playback) Latency() time.Duration { return pcm.Duration(p.latency, OutRate) }

// Flush plays out everything put so far. Call it when no more audio is
// coming for now: when the device runs at another rate, SDL holds the last
// few frames back for its resampler until more audio or a flush comes, so
// without one Queued never returns to 0.
func (p *Playback) Flush() error { return p.s.Flush() }

// Clear drops everything queued, for barge-in, and returns how many bytes
// were dropped. The dropped audio will never be heard, so it no longer
// counts as written. What the device already holds still plays out (at most
// Latency).
func (p *Playback) Clear() int64 {
	// Held across both calls so the device can't take bytes in between
	// (SDL's stream lock is recursive).
	if err := p.s.Lock(); err != nil {
		return 0
	}
	q := p.Queued()
	err := p.s.Clear()
	p.s.Unlock()
	if err != nil {
		return 0
	}
	p.written.Add(-q)
	return q
}

// Close stops playback.
func (p *Playback) Close() { p.s.Destroy() }

// Device is one audio device as SDL sees it.
type Device struct {
	Name     string
	Rate     int
	Channels int
	Frames   int // the device buffer, in sample frames
}

// Devices lists the playback and recording devices without opening them,
// so no microphone permission is asked for.
func Devices() (playback, recording []Device, driver string, err error) {
	list := func(get func() ([]sdl.AudioDeviceID, error)) ([]Device, error) {
		ids, err := get()
		if err != nil {
			return nil, err
		}
		var ds []Device
		for _, id := range ids {
			d := Device{}
			d.Name, _ = id.Name()
			if spec, frames, err := id.Format(); err == nil {
				d.Rate, d.Channels, d.Frames = int(spec.Freq), int(spec.Channels), int(frames)
			}
			ds = append(ds, d)
		}
		return ds, nil
	}
	if playback, err = list(sdl.GetAudioPlaybackDevices); err != nil {
		return nil, nil, "", err
	}
	if recording, err = list(sdl.GetAudioRecordingDevices); err != nil {
		return nil, nil, "", err
	}
	return playback, recording, sdl.GetCurrentAudioDriver(), nil
}
