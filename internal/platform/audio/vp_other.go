//go:build !darwin

package audio

import (
	"errors"
	"fmt"
)

// OpenVoiceProcessing is macOS only.
func OpenVoiceProcessing() (Mic, Speaker, error) {
	return nil, nil, fmt.Errorf("voice processing: %w (macOS only)", errors.ErrUnsupported)
}
