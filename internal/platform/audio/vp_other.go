//go:build !darwin

package audio

import (
	"context"
	"errors"
	"fmt"
)

// vpEngine exists only on macOS.
type vpEngine struct{}

// OpenVoice is macOS only.
func OpenVoice(cfg VoiceConfig) (*Voice, error) {
	return nil, fmt.Errorf("voice processing: %w (macOS only)", errors.ErrUnsupported)
}

func (*vpEngine) read(context.Context, []float32) error  { return errors.ErrUnsupported }
func (*vpEngine) write(context.Context, []float32) error { return errors.ErrUnsupported }
func (*vpEngine) info() VoiceInfo                        { return VoiceInfo{} }
func (*vpEngine) close()                                 {}

// DefaultDevice is a default device as the system has it.
type DefaultDevice struct {
	Name    string
	Rate    float64
	Rates   []float64
	Running bool
}

// Defaults is macOS only.
func Defaults() (in, out DefaultDevice, err error) { return in, out, errors.ErrUnsupported }

// InputRunning is macOS only.
func InputRunning() (bool, error) { return false, errors.ErrUnsupported }

// VoiceDevices is macOS only.
func VoiceDevices() ([]VoiceDevice, error) { return nil, errors.ErrUnsupported }
