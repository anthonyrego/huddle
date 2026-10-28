package platform

import (
	"testing"

	"github.com/ebitengine/purego/objc"
)

// The queries go through the Objective-C runtime, with AppKit loaded by
// SDL. They must find their classes (or they'd quietly report false) and
// not crash, whatever this machine's settings are.
func TestMotionQueries(t *testing.T) {
	LoadSDL()
	for _, c := range []string{"NSWorkspace", "NSProcessInfo"} {
		if objc.GetClass(c) == 0 {
			t.Fatalf("no %s class", c)
		}
	}
	t.Logf("reduce motion %v, low power %v", ReduceMotion(), LowPowerMode())
}
