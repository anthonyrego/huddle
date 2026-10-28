// Package transcript is the conversation so far as a panel the user can
// scroll: what each side said, in turn, the newest at the bottom. It keeps
// the newest words in view until the user scrolls up, and again once they
// scroll back to the end. Pure.
//
// Copied from an earlier project of the author's.
package transcript

import (
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

// Entry is one side's words, or a mark: a divider where a sitting with
// the board began, its Text the time ("today 15:32").
type Entry struct {
	Who  string // the speaker's label: "You" or "AI"
	Text string
	Live bool // still being said: drawn muted
	Mark bool
}

const (
	pad       = 16
	top       = 46 // the title's band; entries scroll below it
	entryGap  = 14
	labelGap  = 3
	labelSize = 11
	textSize  = 13.5
	thumbW    = 3
)

// View draws the entries in a pane and scrolls them. It implements
// ui.Widget and ui.ViewportFitter.
type View struct {
	entries []Entry
	lays    []entryLay
	width   float32
	vh      float32
	content float32 // the height of every entry, gaps included
	scroll  float32 // how far the entries are scrolled up
	follow  bool    // keep the newest words in view
}

type entryLay struct {
	e        Entry
	laid     bool
	label    bool // the first of a run of entries by one speaker
	who, txt text.Layout
	y, h     float32 // in the scrolled area
}

// New returns an empty transcript that follows the newest words.
func New() *View { return &View{follow: true} }

// SetEntries replaces what the transcript shows. The entries are laid out
// again on the next Measure, only where they changed.
func (v *View) SetEntries(es []Entry) { v.entries = es }

// SetViewport implements ui.ViewportFitter: the transcript fills its pane.
func (v *View) SetViewport(h float32) { v.vh = h }

// Following reports whether the newest words stay in view.
func (v *View) Following() bool { return v.follow }

// Scroll is how far the entries are scrolled up.
func (v *View) Scroll() float32 { return v.scroll }

// view is the height of the scrolled area.
func (v *View) view() float32 { return max(v.vh-top-pad, 0) }

func (v *View) maxScroll() float32 { return max(v.content-v.view(), 0) }

func (v *View) Measure(c *ui.Ctx, width float32) ui.Size {
	if width != v.width {
		v.lays, v.width = nil, width
	}
	v.lays = v.lays[:min(len(v.lays), len(v.entries))]
	tw := max(width-2*pad-thumbW, 40)
	y := float32(0)
	for i, e := range v.entries {
		label := !e.Mark && (i == 0 || v.entries[i-1].Who != e.Who || v.entries[i-1].Mark)
		if i >= len(v.lays) {
			v.lays = append(v.lays, entryLay{})
		}
		el := &v.lays[i]
		if el.e != e || !el.laid {
			el.e, el.laid = e, true
			el.who = c.Text.Layout([]text.Run{{Text: e.Who, Style: text.Style{Font: text.SansSemibold, Size: labelSize}}}, 0)
			st := text.Style{Font: text.SansRegular, Size: textSize}
			if e.Mark {
				st = text.Style{Font: text.SansSemibold, Size: labelSize}
			}
			el.txt = c.Text.Layout([]text.Run{{Text: e.Text, Style: st}}, tw)
		}
		el.label = label
		if i > 0 {
			if label || e.Mark || v.entries[i-1].Mark {
				y += entryGap
			} else {
				y += labelGap * 2
			}
		}
		el.y = y
		el.h = el.txt.Height
		if label {
			el.h += el.who.Height + labelGap
		}
		y += el.h
	}
	v.content = y
	if v.follow {
		v.scroll = v.maxScroll()
	}
	v.scroll = min(max(v.scroll, 0), v.maxScroll())
	return ui.Size{W: width, H: v.vh}
}

func (v *View) Draw(c *ui.Ctx, l *draw.List, origin draw.Point, visible draw.Rect) {
	pane := draw.Rect{X: origin.X, Y: origin.Y, W: v.width, H: v.vh}
	l.Rect(pane, c.Pal.Surface, draw.Radii{})
	l.Rect(draw.Rect{X: pane.X, Y: pane.Y, W: 1, H: pane.H}, c.Pal.Border, draw.Radii{})
	title := c.Text.Layout([]text.Run{{Text: "Transcript", Style: text.Style{Font: text.SansSemibold, Size: 13}}}, 0)
	put(l, title, pane.X+pad, pane.Y+(top-title.Height)/2, c.Pal.Text)

	area := draw.Rect{X: pane.X, Y: pane.Y + top, W: pane.W, H: v.view()}
	if len(v.entries) == 0 {
		empty := c.Text.Layout([]text.Run{{Text: "Nothing said yet.", Style: text.Style{Font: text.SansRegular, Size: textSize}}}, 0)
		put(l, empty, area.X+pad, area.Y, c.Pal.TextFaint)
		return
	}
	l.PushClip(area)
	for _, el := range v.lays {
		y := area.Y + el.y - v.scroll
		if y+el.h < area.Y || y > area.Y+area.H {
			continue
		}
		if el.e.Mark {
			v.drawMark(c, l, el, area, y)
			continue
		}
		if el.label {
			put(l, el.who, area.X+pad, y, c.Pal.TextFaint)
			y += el.who.Height + labelGap
		}
		col := c.Pal.Text
		if el.e.Live {
			col = c.Pal.TextMuted
		}
		put(l, el.txt, area.X+pad, y, col)
	}
	l.PopClip()

	if ms := v.maxScroll(); ms > 0 {
		h := max(area.H*area.H/v.content, 24)
		y := area.Y + (area.H-h)*v.scroll/ms
		l.Rect(draw.Rect{X: pane.X + pane.W - thumbW - 4, Y: y, W: thumbW, H: h}, c.Pal.TextFaint.WithAlpha(0x80), draw.Uniform(thumbW/2))
	}
}

// drawMark draws a sitting's time centered between two rules.
func (v *View) drawMark(c *ui.Ctx, l *draw.List, el entryLay, area draw.Rect, y float32) {
	const gap = 8
	x := area.X + (area.W-el.txt.Width)/2
	put(l, el.txt, x, y, c.Pal.TextFaint)
	mid := y + el.txt.Height/2
	left, right := area.X+pad, area.X+area.W-pad
	if w := x - gap - left; w > 0 {
		l.Rect(draw.Rect{X: left, Y: mid, W: w, H: 1}, c.Pal.Border, draw.Radii{})
	}
	if w := right - (x + el.txt.Width + gap); w > 0 {
		l.Rect(draw.Rect{X: x + el.txt.Width + gap, Y: mid, W: w, H: 1}, c.Pal.Border, draw.Radii{})
	}
}

// Event scrolls with the wheel. Scrolled to the end, the transcript
// follows the newest words again.
func (v *View) Event(c *ui.Ctx, ev ui.Event) ui.Result {
	if ev.Kind != ui.Wheel {
		return ui.Result{}
	}
	ms := v.maxScroll()
	v.scroll = min(max(v.scroll+ev.DY, 0), ms)
	v.follow = v.scroll >= ms-1
	return ui.Result{Handled: true, Redraw: true}
}

// Hit implements ui.Widget: the transcript has no addressable parts.
func (v *View) Hit(c *ui.Ctx, p draw.Point) (ui.Part, bool) { return ui.Part{}, false }

// Locate implements ui.Widget.
func (v *View) Locate(c *ui.Ctx, part ui.Part) (draw.Rect, bool) { return draw.Rect{}, false }

// put draws a layout recolored with its top-left corner at x, y.
func put(l *draw.List, lay text.Layout, x, y float32, col draw.Color) {
	for _, line := range lay.Lines {
		for _, run := range line.Runs {
			run.Origin.X += x
			run.Origin.Y += y
			run.Color = col
			l.Glyphs(run)
		}
	}
}
