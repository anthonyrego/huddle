//go:build !darwin

package main

import (
	"context"
	"errors"

	"github.com/anthonyrego/huddle/internal/rtc"
)

// runCam needs VideoToolbox to encode.
func runCam(context.Context, rtc.Join, bool) error {
	return errors.New("bot cam needs macOS (it encodes with VideoToolbox)")
}
