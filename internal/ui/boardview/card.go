package boardview

import (
	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/measure"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// Cards are measured by package measure, with the embedded fonts only, so
// they come out the size the server laid them out at.
const (
	cardRadius = measure.CardRadius
	satRadius  = measure.SatRadius
	satIndent  = measure.SatIndent
)

// kindColor is a kind's accent: the card's border and tag.
func kindColor(p *theme.Palette, k board.Kind) draw.Color {
	switch k {
	case board.Option, board.Decision:
		return p.Warning
	case board.Person:
		return p.Info
	case board.Example, board.Tool:
		return p.TextMuted
	case board.Fact, board.Data:
		return p.Link
	case board.Question, board.Gap:
		return p.TextFaint
	case board.Con, board.Pain:
		return p.Error
	case board.Pro, board.AIOpportunity:
		return p.Success
	}
	return p.NodeStroke
}

func style(font draw.FontID, size float32) text.Style {
	return text.Style{Font: font, Size: size}
}

func lay(m text.Measurer, s string, st text.Style, maxW float32) text.Layout {
	return m.Layout([]text.Run{{Text: s, Style: st}}, maxW)
}

// look is how a card is drawn this frame.
type look struct {
	alpha    float32 // fading in
	hover    string  // the hovered id: the card's or one of its satellites
	textSize func(float32) float32
	halo     draw.Color // under the stems, over the desktop (zero: none)
}

func fade(c draw.Color, a float32) draw.Color {
	return c.WithAlpha(uint8(float32(c.A) * a))
}

// drawCard draws a card with its box at b (b.H is the card's own height).
//
// Copied from an earlier project of the author's.
func drawCard(l *draw.List, p *theme.Palette, c *measure.Card, id string, b draw.Rect, lk look) {
	a := lk.alpha
	accent := kindColor(p, c.Kind)
	l.Shadow(draw.Rect{X: b.X, Y: b.Y + 1.5, W: b.W, H: b.H}, fade(p.Shadow, a), draw.Uniform(cardRadius), 10)
	stroke, width := accent, float32(1.25)
	if c.Kind == board.Idea || c.Kind == board.Step {
		stroke = p.NodeStroke
	}
	if lk.hover == id {
		stroke, width = p.Accent, 2
	}
	if c.Question {
		l.Rect(b, fade(p.Surface, a), draw.Uniform(cardRadius))
		l.Path(draw.Path{Points: roundedOutline(b, cardRadius), Closed: true, Stroke: fade(stroke, a), Width: width, Dashed: true})
	} else {
		l.BorderedRect(b, fade(p.NodeFill, a), fade(stroke, a), width, draw.Uniform(cardRadius))
	}
	x, y := b.X+measure.CardPadX, b.Y+measure.CardPadY
	dot := draw.Rect{X: x, Y: y + c.Tag.Height/2 - 3, W: 6, H: 6}
	l.Rect(dot, fade(accent, a), draw.Uniform(3))
	putText(l, c.Tag, x+10, y, fade(p.TextMuted, a), lk.textSize)
	y += c.Tag.Height + measure.TagGap
	labelCol := p.Text
	if c.Question {
		labelCol = p.TextMuted
	}
	putText(l, c.Label, x, y, fade(labelCol, a), lk.textSize)
	y += c.Label.Height
	for _, ln := range c.Lines {
		y += measure.LineGap
		putText(l, ln, x, y, fade(p.TextMuted, a), lk.textSize)
		y += ln.Height
	}

	for _, s := range c.Sats {
		r := s.Rect(b)
		col := kindColor(p, s.Kind)
		// A short stem from the card to the satellite.
		stem := draw.Point{X: b.X + satIndent/2, Y: r.Y + r.H/2}
		pts := []draw.Point{{X: stem.X, Y: b.Bottom()}, stem, {X: r.X, Y: stem.Y}}
		if lk.halo.A > 0 {
			l.Path(draw.Path{Points: pts, Stroke: fade(lk.halo, a), Width: haloW - 0.6})
		}
		l.Path(draw.Path{Points: pts, Stroke: fade(p.EdgeStroke, a), Width: 1})
		fill := col.WithAlpha(0x1c)
		if p.Dark {
			fill = col.WithAlpha(0x2a)
		}
		border := col.WithAlpha(0x90)
		if lk.hover == s.ID {
			border = p.Accent
		}
		l.Rect(r, fade(p.Surface, a), draw.Uniform(satRadius))
		l.BorderedRect(r, fade(fill, a), fade(border, a), 1, draw.Uniform(satRadius))
		putText(l, s.Tag, r.X+measure.SatPadX, r.Y+measure.SatPadY, fade(col, a), lk.textSize)
		putText(l, s.Label, r.X+measure.SatPadX, r.Y+measure.SatPadY+s.Tag.Height+3, fade(p.Text, a), lk.textSize)
	}
}

// drawNotes draws the meeting's notes panel at r.
func drawNotes(l *draw.List, p *theme.Palette, n *measure.Notes, r draw.Rect, a float32, textSize func(float32) float32) {
	l.Shadow(draw.Rect{X: r.X, Y: r.Y + 1.5, W: r.W, H: r.H}, fade(p.Shadow, a), draw.Uniform(cardRadius), 10)
	l.BorderedRect(r, fade(p.Surface, a), fade(p.Border, a), 1, draw.Uniform(cardRadius))
	x := r.X + measure.NotesPad
	putText(l, n.Title, x, r.Y+measure.NotesPad, fade(p.TextMuted, a), textSize)
	for i, ln := range n.Lines {
		y := r.Y + n.Y[i]
		first := ln.Height
		if len(ln.Lines) > 0 {
			first = ln.Lines[0].Height
		}
		l.Rect(draw.Rect{X: x + 1, Y: y + first/2 - 2.5, W: 5, H: 5}, fade(p.Accent, a), draw.Uniform(2.5))
		putText(l, ln, x+measure.NotesBullet, y, fade(p.Text, a), textSize)
	}
}

// roundedOutline is a rounded rect as a closed polyline, for dashed
// borders.
func roundedOutline(r draw.Rect, rad float32) []draw.Point {
	const steps = 6
	corners := [4]struct{ cx, cy, a0 float32 }{
		{r.Right() - rad, r.Y + rad, -90},
		{r.Right() - rad, r.Bottom() - rad, 0},
		{r.X + rad, r.Bottom() - rad, 90},
		{r.X + rad, r.Y + rad, 180},
	}
	var pts []draw.Point
	for _, c := range corners {
		for i := 0; i <= steps; i++ {
			ang := (c.a0 + 90*float32(i)/steps) * 3.14159265 / 180
			pts = append(pts, draw.Point{X: c.cx + rad*cos(ang), Y: c.cy + rad*sin(ang)})
		}
	}
	return pts
}

// putText draws a layout in a color, at snapped sizes while zooming.
//
// Copied from an earlier project of the author's.
func putText(l *draw.List, lay text.Layout, x, y float32, col draw.Color, textSize func(float32) float32) {
	for _, ln := range lay.Lines {
		for _, run := range ln.Runs {
			run.Origin.X += x
			run.Origin.Y += y
			run.Color = col
			if textSize != nil {
				run.Size = textSize(run.Size)
			}
			l.Glyphs(run)
		}
	}
}
