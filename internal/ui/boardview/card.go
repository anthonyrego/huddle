package boardview

import (
	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// Card geometry, in board units.
const (
	cardRadius = 10
	cardPadX   = 14
	cardPadY   = 11
	cardMinW   = 132
	cardTextW  = 196 // labels wrap at this width
	tagSize    = 10.5
	labelSize  = 15
	lineSize   = 12
	tagGap     = 5
	lineGap    = 4

	satGap    = 8 // between a card and its first satellite, and between satellites
	satIndent = 12
	satRadius = 8
	satPadX   = 10
	satPadY   = 7
	satTextW  = 176
	satSize   = 12.5
)

// tags are the words over a card, for people who don't know the kinds.
var tags = map[board.Kind]string{
	board.Idea:     "idea",
	board.Step:     "step",
	board.Option:   "option",
	board.Person:   "person",
	board.Fact:     "fact",
	board.Question: "open question",
	board.Example:  "example",
	board.Pro:      "pro",
	board.Con:      "con",

	// Retired kinds, for saved boards that use them.
	board.Decision:      "decision",
	board.Tool:          "tool",
	board.Data:          "info",
	board.Gap:           "to ask",
	board.Pain:          "slow part",
	board.AIOpportunity: "AI could help",
}

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

// card is one laid-out box and what hangs under it, measured once per
// content.
type card struct {
	key     string
	kind    board.Kind
	tag     text.Layout
	label   text.Layout
	lines   []text.Layout // detail, then the note
	italic  []bool
	w, h    float32 // the box itself
	sats    []*sat
	totalH  float32 // box plus satellites: what the layout places
	gapCard bool    // an open question: dashed, its label italic and muted
}

// sat is a satellite: a pro, a con or an example under its box.
type sat struct {
	id    string
	kind  board.Kind
	tag   text.Layout
	label text.Layout
	y     float32 // top, from the card's top
	w, h  float32
}

func style(font draw.FontID, size float32) text.Style {
	return text.Style{Font: font, Size: size}
}

func lay(m text.Measurer, s string, st text.Style, maxW float32) text.Layout {
	return m.Layout([]text.Run{{Text: s, Style: st}}, maxW)
}

// cardKey is everything a card's measurement depends on.
func cardKey(n *board.Node, sats []*board.Node) string {
	k := string(n.Kind) + "\x00" + n.Label + "\x00" + n.Detail + "\x00" + n.Note
	for _, s := range sats {
		k += "\x01" + s.ID + "\x00" + string(s.Kind) + "\x00" + s.Label
	}
	return k
}

func measureCard(m text.Measurer, n *board.Node, sats []*board.Node) *card {
	c := &card{key: cardKey(n, sats), kind: n.Kind, gapCard: n.Kind == board.Question || n.Kind == board.Gap}
	c.tag = lay(m, tags[n.Kind], style(text.SansSemibold, tagSize), 0)
	labelFont := text.SansSemibold
	if c.gapCard {
		labelFont = text.SansItalic
	}
	c.label = lay(m, n.Label, style(labelFont, labelSize), cardTextW)
	for i, s := range []string{n.Detail, n.Note} {
		if s == "" {
			continue
		}
		font := text.SansRegular
		if i == 1 {
			font = text.SansItalic
		}
		c.lines = append(c.lines, lay(m, s, style(font, lineSize), cardTextW))
		c.italic = append(c.italic, i == 1)
	}
	w := max(c.tag.Width, c.label.Width)
	h := cardPadY + c.tag.Height + tagGap + c.label.Height
	for _, l := range c.lines {
		w = max(w, l.Width)
		h += lineGap + l.Height
	}
	c.w = max(cardMinW, w+2*cardPadX)
	c.h = h + cardPadY

	y := c.h
	for _, s := range sats {
		st := &sat{id: s.ID, kind: s.Kind}
		st.tag = lay(m, tags[s.Kind], style(text.SansSemibold, tagSize), 0)
		st.label = lay(m, s.Label, style(text.SansRegular, satSize), satTextW)
		st.w = max(st.tag.Width, st.label.Width) + 2*satPadX
		st.h = satPadY + st.tag.Height + 3 + st.label.Height + satPadY
		y += satGap
		st.y = y
		y += st.h
		c.w = max(c.w, st.w+satIndent)
		c.sats = append(c.sats, st)
	}
	c.totalH = y
	return c
}

// satRect is a satellite's box for a card drawn at b (b's height is the
// card's own).
func (s *sat) rect(b draw.Rect) draw.Rect {
	return draw.Rect{X: b.X + satIndent, Y: b.Y + s.y, W: s.w, H: s.h}
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

// draw draws the card with its box at b (b.H is the card's own height).
func (c *card) draw(l *draw.List, p *theme.Palette, id string, b draw.Rect, lk look) {
	a := lk.alpha
	accent := kindColor(p, c.kind)
	l.Shadow(draw.Rect{X: b.X, Y: b.Y + 1.5, W: b.W, H: b.H}, fade(p.Shadow, a), draw.Uniform(cardRadius), 10)
	stroke, width := accent, float32(1.25)
	if c.kind == board.Idea || c.kind == board.Step {
		stroke = p.NodeStroke
	}
	if lk.hover == id {
		stroke, width = p.Accent, 2
	}
	if c.gapCard {
		l.Rect(b, fade(p.Surface, a), draw.Uniform(cardRadius))
		l.Path(draw.Path{Points: roundedOutline(b, cardRadius), Closed: true, Stroke: fade(stroke, a), Width: width, Dashed: true})
	} else {
		l.BorderedRect(b, fade(p.NodeFill, a), fade(stroke, a), width, draw.Uniform(cardRadius))
	}
	x, y := b.X+cardPadX, b.Y+cardPadY
	dot := draw.Rect{X: x, Y: y + c.tag.Height/2 - 3, W: 6, H: 6}
	l.Rect(dot, fade(accent, a), draw.Uniform(3))
	putText(l, c.tag, x+10, y, fade(p.TextMuted, a), lk.textSize)
	y += c.tag.Height + tagGap
	labelCol := p.Text
	if c.gapCard {
		labelCol = p.TextMuted
	}
	putText(l, c.label, x, y, fade(labelCol, a), lk.textSize)
	y += c.label.Height
	for _, ln := range c.lines {
		y += lineGap
		putText(l, ln, x, y, fade(p.TextMuted, a), lk.textSize)
		y += ln.Height
	}

	for _, s := range c.sats {
		r := s.rect(b)
		col := kindColor(p, s.kind)
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
		if lk.hover == s.id {
			border = p.Accent
		}
		l.Rect(r, fade(p.Surface, a), draw.Uniform(satRadius))
		l.BorderedRect(r, fade(fill, a), fade(border, a), 1, draw.Uniform(satRadius))
		putText(l, s.tag, r.X+satPadX, r.Y+satPadY, fade(col, a), lk.textSize)
		putText(l, s.label, r.X+satPadX, r.Y+satPadY+s.tag.Height+3, fade(p.Text, a), lk.textSize)
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
