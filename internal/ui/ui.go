// Package ui is the in-window interface: input events, the Widget contract
// the designer implements, the Host that shows one widget in an area of the
// window, keyboard focus layers and the TextField.
//
// It is pure Go and never imports SDL: the app translates SDL events into
// Events, and the same widget renders in the window and offscreen.
// Coordinates are logical units, y down, like package draw.
//
// Copied from an earlier project of the author's (without
// the component tree, containers and Choice).
package ui

import (
	"time"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// EventKind identifies an Event.
type EventKind uint8

const (
	PointerMove EventKind = iota
	PointerDown
	PointerUp
	PointerLeave // the pointer left the widget or the window
	Wheel        // DX/DY in logical units, positive = content moves up/left
	KeyPress
	KeyRelease
	TextInput // committed text from the IME or keyboard, in Text
	Pinch     // a trackpad pinch step at Pos; Scale is the change (1.02 = 2% larger)
)

// Mods is a set of held modifier keys.
type Mods uint8

const (
	Shift Mods = 1 << iota
	Ctrl
	Alt
	Cmd // ⌘ on macOS, the Windows/Super key elsewhere
)

// Key is a key code: printable keys are their unshifted lowercase rune
// ('a', '1', '[', ' '); other keys use the constants below.
type Key rune

const (
	KeyNone  Key = 0
	KeyEnter Key = 0x10FFFF - iota
	KeyEscape
	KeyTab
	KeyBackspace
	KeyDelete
	KeyArrowUp
	KeyArrowDown
	KeyArrowLeft
	KeyArrowRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
)

// Event is one input event. Positions are in the receiver's coordinates:
// the app delivers window coordinates, and the Host translates them into
// its widget's content coordinates.
type Event struct {
	Kind   EventKind
	Pos    draw.Point
	Button int // 1 left, 2 middle, 3 right (pointer events)
	Clicks int // 1 single, 2 double… (PointerDown)
	// DX, DY are the scroll deltas for Wheel. Precise is set for trackpads
	// and other pixel-precise devices; notched wheels are pre-scaled to
	// logical units by the app.
	DX, DY  float32
	Precise bool
	Key     Key
	Repeat  bool
	Mods    Mods
	Text    string
	Scale   float32 // Pinch
}

// Result is what a widget did with an Event.
type Result struct {
	// Handled says the widget used the event; what it leaves unhandled is
	// the app's (shortcuts).
	Handled bool
	// Redraw asks for a new frame.
	Redraw bool
}

// Size is a content size in logical units.
type Size struct{ W, H float32 }

// Ctx is what widgets get for measuring, drawing and handling events. It is
// only used on the main thread.
type Ctx struct {
	Text  text.Measurer
	Pal   theme.Palette
	Scale float32 // pixels per logical unit, for snapping hairlines
	Now   time.Time
	// Animate asks for another frame after this one (a fading highlight,
	// smooth scroll). Widgets call it each frame they are still moving.
	Animate func()
}

// RequestFrame calls c.Animate if it is set.
func (c *Ctx) RequestFrame() {
	if c.Animate != nil {
		c.Animate()
	}
}

// Widget is something that draws content of some size and handles input
// over it: the tree designer. The Host that holds it owns the viewport and
// the clip.
type Widget interface {
	// Measure lays content out for a viewport width and returns its size.
	// It is called when the width, palette or scale changes and before the
	// first Draw; widgets cache whatever they need for that width.
	Measure(c *Ctx, width float32) Size

	// Draw appends the content with its top-left corner at origin (window
	// coordinates, already offset by the scroll position). visible is the
	// part of the content, in content coordinates, that is on screen:
	// long content draws only what intersects it. The container has
	// already pushed a clip for the viewport.
	Draw(c *Ctx, l *draw.List, origin draw.Point, visible draw.Rect)

	// Event handles input. Positions are content coordinates.
	Event(c *Ctx, ev Event) Result

	// Hit returns the part under p (content coordinates), for comments and
	// hover. ok is false where there is no addressable part.
	Hit(c *Ctx, p draw.Point) (part Part, ok bool)

	// Locate returns the content rect of a part (where a comment box
	// anchors). ok is false for parts the widget doesn't have.
	Locate(c *Ctx, part Part) (r draw.Rect, ok bool)
}
