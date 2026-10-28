package ui

import (
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// fakeText lays text out at 7 units per rune and 16 per line.
type fakeText struct{}

func (fakeText) Layout(runs []text.Run, maxW float32) text.Layout {
	var s string
	for _, r := range runs {
		s += r.Text
	}
	w := float32(len([]rune(s))) * 7
	lines := 1
	if maxW > 0 && w > maxW {
		lines = int(w/maxW) + 1
		w = maxW
	}
	return text.Layout{Width: w, Height: float32(lines) * 16, Lines: make([]text.Line, lines)}
}

// box is a test widget that logs what it saw and makes an edit per click.
type box struct {
	vh     float32
	width  float32
	events []Event
	edits  []any
}

func (b *box) SetViewport(h float32)                        { b.vh = h }
func (b *box) Measure(c *Ctx, width float32) Size           { b.width = width; return Size{W: width, H: b.vh} }
func (b *box) Draw(*Ctx, *draw.List, draw.Point, draw.Rect) {}
func (b *box) Event(c *Ctx, ev Event) Result {
	b.events = append(b.events, ev)
	if ev.Kind == PointerUp {
		b.edits = append(b.edits, "click")
	}
	return Result{Handled: true}
}
func (b *box) Hit(c *Ctx, p draw.Point) (Part, bool) {
	return Part{Kind: PartNode, ID: "x"}, p.X < 50
}
func (b *box) Locate(*Ctx, Part) (draw.Rect, bool) { return draw.Rect{X: 1, Y: 2, W: 3, H: 4}, true }
func (b *box) TakeEdits() []any                    { e := b.edits; b.edits = nil; return e }

func TestHostRoutesInContentCoordinates(t *testing.T) {
	c := &Ctx{Text: fakeText{}, Pal: theme.Light, Scale: 2}
	b := &box{}
	var edits []any
	h := &Host{W: b, OnEdit: func(e any) { edits = append(edits, e) }}
	var l draw.List
	h.Draw(c, &l, draw.Rect{X: 0, Y: 40, W: 300, H: 200})
	if b.vh != 200 || b.width != 300 {
		t.Fatalf("measured %v×%v, want the area", b.width, b.vh)
	}

	h.Event(c, Event{Kind: PointerMove, Pos: draw.Point{X: 10, Y: 20}}) // over the top bar: nothing
	if len(b.events) != 0 {
		t.Fatalf("got %v outside the area", b.events)
	}
	h.Event(c, Event{Kind: PointerDown, Pos: draw.Point{X: 10, Y: 50}})
	h.Event(c, Event{Kind: PointerMove, Pos: draw.Point{X: 10, Y: 5}}) // dragged out: still the widget's
	h.Event(c, Event{Kind: PointerUp, Pos: draw.Point{X: 10, Y: 5}})
	if len(b.events) != 3 || b.events[0].Pos != (draw.Point{X: 10, Y: 10}) || b.events[1].Pos.Y != -35 {
		t.Fatalf("events %+v", b.events)
	}
	if len(edits) != 1 {
		t.Fatalf("edits %v, want the click's", edits)
	}
	h.Event(c, Event{Kind: PointerMove, Pos: draw.Point{X: 10, Y: 60}})
	h.Event(c, Event{Kind: PointerMove, Pos: draw.Point{X: 10, Y: 5}})
	if last := b.events[len(b.events)-1]; last.Kind != PointerLeave {
		t.Fatalf("leaving the area gave %+v, want PointerLeave", last)
	}

	if p, ok := h.Hit(c, draw.Point{X: 10, Y: 60}); !ok || p.String() != "node:x" {
		t.Fatalf("hit %v %v", p, ok)
	}
	if r, _ := h.PartRect(c, Part{Kind: PartNode, ID: "x"}); r.Y != 42 {
		t.Fatalf("part rect %+v, want it in window coordinates", r)
	}
}
