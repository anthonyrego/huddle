package platform

import (
	"strconv"

	"github.com/ebitengine/purego/objc"
)

// SetDockBadge shows n on the app's Dock icon; 0 clears it. Call it on the
// main thread.
func SetDockBadge(n int) {
	app := objc.ID(objc.GetClass("NSApplication")).Send(objc.RegisterName("sharedApplication"))
	if app == 0 {
		return
	}
	tile := app.Send(objc.RegisterName("dockTile"))
	if tile == 0 {
		return
	}
	var label objc.ID // nil clears the badge
	if n > 0 {
		label = objc.ID(objc.GetClass("NSString")).Send(objc.RegisterName("alloc")).
			Send(objc.RegisterName("initWithUTF8String:"), strconv.Itoa(n))
		defer label.Send(objc.RegisterName("release"))
	}
	tile.Send(objc.RegisterName("setBadgeLabel:"), label)
}
