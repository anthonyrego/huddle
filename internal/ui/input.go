package ui

import "github.com/anthonyrego/huddle/internal/draw"

// Copier is implemented by what holds a text selection (a field, a widget
// with fields, a layer): Copy returns the selected text, and with cut
// removes it. The app puts it on the clipboard for cmd-C and cmd-X.
type Copier interface {
	Copy(cut bool) (string, bool)
}

// CaretReporter is an optional Widget interface for widgets that take
// text: where the caret is (content coordinates), for the IME's candidate
// window.
type CaretReporter interface {
	CaretRect() (draw.Rect, bool)
}

// Composer is a Layer that shows the IME's composition (marked text)
// while it takes text.
type Composer interface {
	Compose(text string, cursor int)
}

// TextAreaReporter is a Layer that says where its caret is (window
// coordinates), for the IME's candidate window.
type TextAreaReporter interface {
	TextArea() (draw.Rect, bool)
}

// TextTaker is implemented by widgets that take typed text while focused
// (the designer's inspector). The app turns on text input and the IME for
// them and sends TextInput events.
type TextTaker interface{ WantsText() bool }

// Layer is something keyboard input goes to: the designer's Host or an
// overlay the app puts on top of it (the comment box). While an overlay is
// on top it gets every key and text event.
type Layer interface {
	Key(c *Ctx, ev Event) Result
	// WantsText reports whether the layer takes committed text; the app
	// turns on text input and the IME while it does.
	WantsText() bool
}

// Focus routes keyboard input: to the top overlay if there is one, else to
// the base layer (the Host).
type Focus struct {
	Base     Layer
	overlays []Layer
}

// Push puts an overlay on top.
func (f *Focus) Push(l Layer) {
	f.Remove(l)
	f.overlays = append(f.overlays, l)
}

// Remove takes an overlay off, wherever it is.
func (f *Focus) Remove(l Layer) {
	for i, o := range f.overlays {
		if o == l {
			f.overlays = append(f.overlays[:i], f.overlays[i+1:]...)
			return
		}
	}
}

// Top is the layer keys go to (nil when there is none).
func (f *Focus) Top() Layer {
	if n := len(f.overlays); n > 0 {
		return f.overlays[n-1]
	}
	return f.Base
}

// Modal reports whether an overlay has the keyboard.
func (f *Focus) Modal() bool { return len(f.overlays) > 0 }

// Key delivers a key or text event to the top layer.
func (f *Focus) Key(c *Ctx, ev Event) Result {
	if t := f.Top(); t != nil {
		return t.Key(c, ev)
	}
	return Result{}
}

// WantsText reports whether the top layer takes text.
func (f *Focus) WantsText() bool {
	t := f.Top()
	return t != nil && t.WantsText()
}

// ViewportFitter is an optional Widget interface for content that sizes
// itself to the viewport it's shown in (the designer fills its area).
// Before measuring, the Host tells it the area's height.
type ViewportFitter interface {
	SetViewport(h float32)
}

// EditSource is an optional Widget interface for widgets the user edits
// the content of (the designer). After each event the Host takes the edits
// it made and hands them to Host.OnEdit.
type EditSource interface {
	TakeEdits() []any
}

// PointerCursor is an optional Widget interface: a widget whose
// PointerCursor reports true after a pointer event gets the pointing-hand
// cursor.
type PointerCursor interface {
	PointerCursor() bool
}

// Cursor is the mouse cursor the window wants.
type Cursor uint8

const (
	CursorDefault Cursor = iota
	CursorPointer
	CursorResizeH
	CursorResizeV
	CursorText // over a text field
)
