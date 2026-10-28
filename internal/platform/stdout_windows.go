//go:build windows

package platform

import "os"

// ReserveStdout hands the real stdout to the caller and points os.Stdout at
// stderr. Unlike the unix version this only redirects Go writes; native code
// writing to the console handle is not redirected.
func ReserveStdout() (*os.File, error) {
	out := os.Stdout
	os.Stdout = os.Stderr
	return out, nil
}
