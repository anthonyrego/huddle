//go:build !darwin

package platform

// ReduceMotion reports whether the OS asks apps to reduce motion. Only macOS
// is asked.
func ReduceMotion() bool { return false }

// LowPowerMode reports whether the OS is saving power. Only macOS is asked.
func LowPowerMode() bool { return false }
