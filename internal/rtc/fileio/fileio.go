// Package fileio is a mic and a speaker made of files, for running voice
// with no audio devices: the mic plays a recording in real time, the
// speaker records what it is given in real time. Both pace themselves on
// the wall clock, 10 ms a frame, as devices would, and remember when they
// started so recordings made in one process can be lined up.
package fileio

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/rtc"
)

const frameDur = time.Second * rtc.FrameSamples / rtc.SampleRate

// ReadWAV reads a mono s16 WAV at 48 kHz as float32.
func ReadWAV(path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, rate, err := pcm.ReadWAV(f)
	if err != nil {
		return nil, err
	}
	if rate != rtc.SampleRate {
		return nil, &rateError{path, rate}
	}
	return pcm.F32FromS16(data), nil
}

type rateError struct {
	path string
	rate int
}

func (e *rateError) Error() string {
	return e.path + ": " + strconv.Itoa(e.rate) + " Hz, want 48000 (ffmpeg -i in -ar 48000 -ac 1 out.wav)"
}

// WriteWAV writes float32 samples as a mono s16 WAV at 48 kHz.
func WriteWAV(path string, s []float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := pcm.WriteWAV(f, pcm.S16FromF32(s), rtc.SampleRate); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Mic plays samples as a microphone would capture them: frame k is ready
// 10 ms after frame k-1, then silence once they run out.
type Mic struct {
	Samples []float32

	mu    sync.Mutex
	start time.Time
	k     int64
}

// Read waits for the next frame and fills it.
func (m *Mic) Read(ctx context.Context, frame []float32) error {
	m.mu.Lock()
	if m.start.IsZero() {
		m.start = time.Now()
	}
	// A frame is captured by the end of its 10 ms.
	due := m.start.Add(time.Duration(m.k+1) * frameDur)
	pos := int(m.k) * rtc.FrameSamples
	m.k++
	m.mu.Unlock()
	if err := sleepUntil(ctx, due); err != nil {
		return err
	}
	clear(frame)
	if pos < len(m.Samples) {
		copy(frame, m.Samples[pos:])
	}
	return nil
}

// Start is when the mic's first sample was captured (zero before the
// first Read).
func (m *Mic) Start() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.start
}

// Done reports whether every sample has been read.
func (m *Mic) Done() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int(m.k)*rtc.FrameSamples >= len(m.Samples)
}

// Speaker records what it is given: frame k plays 10 ms after frame k-1,
// and Write blocks until it is time.
type Speaker struct {
	mu      sync.Mutex
	start   time.Time
	k       int64
	samples []float32
}

// Write waits for the frame's turn and records it.
func (s *Speaker) Write(ctx context.Context, frame []float32) error {
	s.mu.Lock()
	if s.start.IsZero() {
		s.start = time.Now()
	}
	due := s.start.Add(time.Duration(s.k) * frameDur)
	s.k++
	s.mu.Unlock()
	if err := sleepUntil(ctx, due); err != nil {
		return err
	}
	s.mu.Lock()
	s.samples = append(s.samples, frame...)
	s.mu.Unlock()
	return nil
}

// Start is when the first recorded sample played.
func (s *Speaker) Start() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start
}

// Samples is everything recorded so far.
func (s *Speaker) Samples() []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]float32(nil), s.samples...)
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}
