package ui

import "github.com/anthonyrego/huddle/internal/draw"

// Host shows one widget filling an area of the window. It measures the
// widget for the area, draws it clipped there and routes input to it in its
// content coordinates; a press it takes keeps the pointer until the release,
// wherever the pointer goes. As the base keyboard Layer it hands keys, the
// IME's composition, the clipboard and the widget's edits through.
type Host struct {
	W Widget
	// OnEdit receives each edit the widget made (EditSource).
	OnEdit func(edit any)

	area     draw.Rect
	captured bool // a press went to the widget; it gets every pointer event until the release
	inside   bool // the pointer is over the area
	cursor   bool // the widget asked for the pointing hand
}

// Area is where the widget was last drawn, in window coordinates.
func (h *Host) Area() draw.Rect { return h.area }

// Draw measures the widget for area and draws it there.
func (h *Host) Draw(c *Ctx, l *draw.List, area draw.Rect) {
	h.area = area
	if h.W == nil {
		return
	}
	if vf, ok := h.W.(ViewportFitter); ok {
		vf.SetViewport(area.H)
	}
	h.W.Measure(c, area.W)
	l.PushClip(area)
	h.W.Draw(c, l, draw.Point{X: area.X, Y: area.Y}, draw.Rect{W: area.W, H: area.H})
	l.PopClip()
}

// Event delivers a pointer, wheel or pinch event (window coordinates) to
// the widget, and key and text events as Key does.
func (h *Host) Event(c *Ctx, ev Event) Result {
	if h.W == nil {
		return Result{}
	}
	switch ev.Kind {
	case KeyPress, KeyRelease, TextInput:
		return h.Key(c, ev)
	}
	in := h.area.Contains(ev.Pos)
	switch ev.Kind {
	case PointerLeave:
		h.inside = false
		if h.captured {
			return Result{}
		}
		return h.deliver(c, ev)
	case PointerDown:
		if !in {
			return Result{}
		}
		h.captured = true
	case PointerUp:
		if !h.captured {
			return Result{}
		}
		h.captured = false
	case PointerMove:
		if !in && !h.captured {
			if h.inside {
				h.inside = false
				return h.deliver(c, Event{Kind: PointerLeave, Pos: ev.Pos})
			}
			return Result{}
		}
	case Wheel, Pinch:
		if !in {
			return Result{}
		}
	}
	h.inside = in
	return h.deliver(c, ev)
}

// deliver hands ev to the widget in content coordinates and takes its
// edits.
func (h *Host) deliver(c *Ctx, ev Event) Result {
	ev.Pos = h.content(ev.Pos)
	res := h.W.Event(c, ev)
	if pc, ok := h.W.(PointerCursor); ok {
		h.cursor = pc.PointerCursor()
	}
	if es, ok := h.W.(EditSource); ok {
		for _, e := range es.TakeEdits() {
			if h.OnEdit != nil {
				h.OnEdit(e)
			}
			res.Redraw = true
		}
	}
	return res
}

func (h *Host) content(p draw.Point) draw.Point {
	return draw.Point{X: p.X - h.area.X, Y: p.Y - h.area.Y}
}

// Key implements Layer: key and text events go to the widget.
func (h *Host) Key(c *Ctx, ev Event) Result {
	if h.W == nil {
		return Result{}
	}
	return h.deliver(c, ev)
}

// WantsText implements Layer.
func (h *Host) WantsText() bool {
	t, ok := h.W.(TextTaker)
	return ok && t.WantsText()
}

// Compose implements Composer.
func (h *Host) Compose(text string, cursor int) {
	if cp, ok := h.W.(Composer); ok {
		cp.Compose(text, cursor)
	}
}

// Copy implements Copier.
func (h *Host) Copy(cut bool) (string, bool) {
	if cp, ok := h.W.(Copier); ok {
		return cp.Copy(cut)
	}
	return "", false
}

// TextArea implements TextAreaReporter: the widget's caret in window
// coordinates.
func (h *Host) TextArea() (draw.Rect, bool) {
	cr, ok := h.W.(CaretReporter)
	if !ok {
		return draw.Rect{}, false
	}
	r, ok := cr.CaretRect()
	if !ok {
		return draw.Rect{}, false
	}
	r.X += h.area.X
	r.Y += h.area.Y
	return r, true
}

// Hit returns the part under p (window coordinates).
func (h *Host) Hit(c *Ctx, p draw.Point) (Part, bool) {
	if h.W == nil || !h.area.Contains(p) {
		return Part{}, false
	}
	return h.W.Hit(c, h.content(p))
}

// PartRect returns a part's rect in window coordinates.
func (h *Host) PartRect(c *Ctx, part Part) (draw.Rect, bool) {
	if h.W == nil {
		return draw.Rect{}, false
	}
	r, ok := h.W.Locate(c, part)
	if !ok {
		return draw.Rect{}, false
	}
	r.X += h.area.X
	r.Y += h.area.Y
	return r, true
}

// Cursor is the cursor the widget wants.
func (h *Host) Cursor() Cursor {
	if h.cursor && (h.inside || h.captured) {
		return CursorPointer
	}
	return CursorDefault
}
