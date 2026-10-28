//go:build !darwin

package platform

// SetDockBadge shows n on the app's Dock icon. Only macOS has one.
func SetDockBadge(n int) {}
