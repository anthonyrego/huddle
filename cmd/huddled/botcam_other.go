//go:build !darwin

package main

import (
	"context"
	"errors"
)

// runCam needs VideoToolbox to encode.
func runCam(context.Context, string, string, bool) error {
	return errors.New("bot cam needs macOS (it encodes with VideoToolbox)")
}
