package audio

import (
	"context"
	"time"
)

// VoiceConfig sets up the voice path's devices. The zero value is the
// voice's defaults: 48 kHz mono both ways, 50 ms queued for the speakers,
// voice processing on.
type VoiceConfig struct {
	// MicRate and SpeakerRate are the rates Read gives and Write takes
	// (float32 mono); 0 is 48000.
	MicRate, SpeakerRate int
	// Queue is how much audio Write keeps scheduled for the speakers; it
	// blocks beyond that. 0 is 50 ms (40–50 ms held in 10 ms frames).
	Queue time.Duration
	// SpeakerOnly opens only the speakers: no mic, no voice processing.
	SpeakerOnly bool
}

func (c VoiceConfig) withDefaults() VoiceConfig {
	if c.MicRate == 0 {
		c.MicRate = 48000
	}
	if c.SpeakerRate == 0 {
		c.SpeakerRate = 48000
	}
	if c.Queue == 0 {
		c.Queue = 50 * time.Millisecond
	}
	return c
}

// Voice is the voice path's mic and speakers (OpenVoice).
type Voice struct{ e *vpEngine }

// VoiceInfo is what the voice path is doing, for logs and audio-devices.
type VoiceInfo struct {
	SpeakerOnly, VoiceProcess bool
	DeviceMic                 int           // the mic device's rate (0 without one)
	Latency                   time.Duration // the output's presentation latency
	Queued                    time.Duration // scheduled on the player, not yet rendered
	Written, Dropped          time.Duration // given to Write since opening; let go because the engine wasn't taking it
	MicHeld                   time.Duration // captured, not yet read
	FirstRender               bool          // the player has rendered a buffer since the engine last started
	EngineAlive               bool
}

// VoiceMic reads the cleaned mic: 10 ms frames of float32 at MicRate,
// evenly paced (rtc.Mic).
type VoiceMic struct{ v *Voice }

// Read blocks until the next frame is due.
func (m VoiceMic) Read(ctx context.Context, frame []float32) error { return m.v.e.read(ctx, frame) }

// VoiceSpeaker plays the mix: float32 at SpeakerRate (rtc.Speaker).
type VoiceSpeaker struct{ v *Voice }

// Write schedules a frame, blocking while Queue's worth waits to be played.
func (s VoiceSpeaker) Write(ctx context.Context, frame []float32) error {
	return s.v.e.write(ctx, frame)
}

// Mic is the voice path's mic (an error to read when SpeakerOnly).
func (v *Voice) Mic() VoiceMic { return VoiceMic{v} }

// Speaker is the voice path's speakers.
func (v *Voice) Speaker() VoiceSpeaker { return VoiceSpeaker{v} }

// Info says what the path is doing now.
func (v *Voice) Info() VoiceInfo { return v.e.info() }

// Close stops the engine and puts the speakers back as they were.
func (v *Voice) Close() { v.e.close() }
