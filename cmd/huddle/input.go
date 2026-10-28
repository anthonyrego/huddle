// Copied from an earlier project of the author's.
package main

import (
	"math"
	"runtime"
	"unsafe"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/ui"
)

// wheelScale converts SDL wheel units to logical units. On macOS SDL
// reports trackpad (precise) deltas in points times 0.1 and notched wheels
// in AppKit's accelerated lines, and AppKit scrolls 10 points per line, so
// 10 tracks the fingers and matches native wheel scrolling. Elsewhere a
// notch is about three lines.
func wheelScale() float32 {
	if runtime.GOOS == "darwin" {
		return 10
	}
	return 40
}

// uiMods converts SDL modifiers.
func uiMods(m sdl.Keymod) ui.Mods {
	var out ui.Mods
	if m&sdl.KMOD_SHIFT != 0 {
		out |= ui.Shift
	}
	if m&sdl.KMOD_CTRL != 0 {
		out |= ui.Ctrl
	}
	if m&sdl.KMOD_ALT != 0 {
		out |= ui.Alt
	}
	if m&sdl.KMOD_GUI != 0 {
		out |= ui.Cmd
	}
	return out
}

// shortcutMod is the modifier app shortcuts use: ⌘ on macOS, Ctrl elsewhere.
func shortcutMod() ui.Mods {
	if runtime.GOOS == "darwin" {
		return ui.Cmd
	}
	return ui.Ctrl
}

var specialKeys = map[sdl.Keycode]ui.Key{
	sdl.K_RETURN:    ui.KeyEnter,
	sdl.K_KP_ENTER:  ui.KeyEnter,
	sdl.K_ESCAPE:    ui.KeyEscape,
	sdl.K_TAB:       ui.KeyTab,
	sdl.K_BACKSPACE: ui.KeyBackspace,
	sdl.K_DELETE:    ui.KeyDelete,
	sdl.K_UP:        ui.KeyArrowUp,
	sdl.K_DOWN:      ui.KeyArrowDown,
	sdl.K_LEFT:      ui.KeyArrowLeft,
	sdl.K_RIGHT:     ui.KeyArrowRight,
	sdl.K_HOME:      ui.KeyHome,
	sdl.K_END:       ui.KeyEnd,
	sdl.K_PAGEUP:    ui.KeyPageUp,
	sdl.K_PAGEDOWN:  ui.KeyPageDown,
}

// uiKey converts a keycode: printable keys are their unshifted rune (SDL
// keycodes for them are the rune), the rest map to ui constants.
func uiKey(k sdl.Keycode) ui.Key {
	if u, ok := specialKeys[k]; ok {
		return u
	}
	if k < 0x40000000 && k >= ' ' {
		return ui.Key(k)
	}
	return ui.KeyNone
}

// translate converts an SDL event to a ui event in logical window
// coordinates (event coordinates times PixelDensity over the display
// scale). ok is false for events the view doesn't take.
func (a *winApp) translate(ev *sdl.Event) (ui.Event, bool) {
	pos := func(x, y float32) draw.Point {
		lx, ly := a.win().ToLogical(x, y)
		return draw.Point{X: lx, Y: ly}
	}
	mods := uiMods(sdl.GetModState())
	switch ev.Type {
	case sdl.EVENT_MOUSE_MOTION:
		m := ev.MouseMotionEvent()
		return ui.Event{Kind: ui.PointerMove, Pos: pos(m.X, m.Y), Mods: mods}, true
	case sdl.EVENT_MOUSE_BUTTON_DOWN, sdl.EVENT_MOUSE_BUTTON_UP:
		b := ev.MouseButtonEvent()
		k := ui.PointerDown
		if ev.Type == sdl.EVENT_MOUSE_BUTTON_UP {
			k = ui.PointerUp
		}
		return ui.Event{Kind: k, Pos: pos(b.X, b.Y), Button: int(b.Button), Clicks: int(b.Clicks), Mods: mods}, true
	case sdl.EVENT_MOUSE_WHEEL:
		w := ev.MouseWheelEvent()
		s := wheelScale()
		precise := w.X != float32(math.Trunc(float64(w.X))) || w.Y != float32(math.Trunc(float64(w.Y)))
		return ui.Event{Kind: ui.Wheel, Pos: pos(w.MouseX, w.MouseY), DX: w.X * s, DY: -w.Y * s, Precise: precise, Mods: mods}, true
	case sdl.EVENT_PINCH_UPDATE:
		// Pinches carry no position; they apply where the pointer is.
		_, x, y := sdl.GetMouseState()
		return ui.Event{Kind: ui.Pinch, Pos: pos(x, y), Scale: pinchScale(ev), Mods: mods}, true
	case sdl.EVENT_WINDOW_MOUSE_LEAVE:
		return ui.Event{Kind: ui.PointerLeave, Mods: mods}, true
	case sdl.EVENT_KEY_DOWN, sdl.EVENT_KEY_UP:
		k := ev.KeyboardEvent()
		kind := ui.KeyPress
		if ev.Type == sdl.EVENT_KEY_UP {
			kind = ui.KeyRelease
		}
		key := uiKey(k.Key)
		if key == ui.KeyNone {
			return ui.Event{}, false
		}
		return ui.Event{Kind: kind, Key: key, Repeat: k.Repeat, Mods: uiMods(k.Mod)}, true
	case sdl.EVENT_TEXT_INPUT:
		return ui.Event{Kind: ui.TextInput, Text: ev.TextInputEvent().Text, Mods: mods}, true
	}
	return ui.Event{}, false
}

// pinchScale reads a pinch event's scale change.
func pinchScale(ev *sdl.Event) float32 {
	return (*sdl.PinchFingerEvent)(unsafe.Pointer(ev)).Scale
}

// cursors are the system cursors, created on first use.
var cursors = map[ui.Cursor]*sdl.Cursor{}

func setCursor(c ui.Cursor) {
	cur, ok := cursors[c]
	if !ok {
		id := sdl.SYSTEM_CURSOR_DEFAULT
		switch c {
		case ui.CursorPointer:
			id = sdl.SYSTEM_CURSOR_POINTER
		case ui.CursorResizeH:
			id = sdl.SYSTEM_CURSOR_EW_RESIZE
		case ui.CursorResizeV:
			id = sdl.SYSTEM_CURSOR_NS_RESIZE
		case ui.CursorText:
			id = sdl.SYSTEM_CURSOR_TEXT
		}
		cur, _ = sdl.CreateSystemCursor(id)
		cursors[c] = cur
	}
	if cur != nil {
		sdl.SetCursor(cur)
	}
}
