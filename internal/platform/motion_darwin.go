package platform

import "github.com/ebitengine/purego/objc"

// ReduceMotion reports whether macOS's Reduce Motion accessibility setting
// is on. Call it on the main thread.
func ReduceMotion() bool {
	ws := objc.ID(objc.GetClass("NSWorkspace")).Send(objc.RegisterName("sharedWorkspace"))
	if ws == 0 {
		return false
	}
	return objc.Send[bool](ws, objc.RegisterName("accessibilityDisplayShouldReduceMotion"))
}

// LowPowerMode reports whether macOS's Low Power Mode is on.
func LowPowerMode() bool {
	info := objc.ID(objc.GetClass("NSProcessInfo")).Send(objc.RegisterName("processInfo"))
	sel := objc.RegisterName("isLowPowerModeEnabled")
	if info == 0 || !objc.Send[bool](info, objc.RegisterName("respondsToSelector:"), sel) {
		return false
	}
	return objc.Send[bool](info, sel)
}
