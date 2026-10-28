// Package measure sizes what the board draws: a card with its label's line
// breaks and the satellites under it, and the meeting's notes panel. It
// shapes with the embedded fonts only (no system fallback), so the server,
// which lays the board out, and every app, which draws it, break every
// line the same way and agree on every size.
//
// Card measurement copied from an earlier project of the author's.
package measure

import (
	"sync"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
)

// Card geometry, in board units.
const (
	CardRadius = 10
	CardPadX   = 14
	CardPadY   = 11
	CardMinW   = 132
	CardTextW  = 196 // labels wrap at this width
	TagSize    = 10.5
	LabelSize  = 15
	LineSize   = 12
	TagGap     = 5
	LineGap    = 4

	SatGap    = 8 // between a card and its first satellite, and between satellites
	SatIndent = 12
	SatRadius = 8
	SatPadX   = 10
	SatPadY   = 7
	SatTextW  = 176
	SatSize   = 12.5

	NotesPad    = 16
	NotesTextW  = 260 // lines wrap at this width
	NotesTitle  = 13
	NotesSize   = 13
	NotesGap    = 6  // between lines
	NotesBullet = 14 // a line's indent after its bullet

	GroupLabelSize = 12.5 // a frame's label
)

// Tags are the words over a card, for people who don't know the kinds.
var Tags = map[board.Kind]string{
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

var (
	shaperOnce sync.Once
	shaper     *text.Shaper
	shaperErr  error
)

// Text is the embedded-fonts-only shaper everything on the board is
// measured with. It is shared and safe for concurrent use.
func Text() text.Measurer {
	shaperOnce.Do(func() { shaper, shaperErr = text.NewEmbeddedShaper() })
	if shaperErr != nil {
		panic("measure: the embedded fonts don't load: " + shaperErr.Error())
	}
	return shaper
}

// Card is one box and what hangs under it, measured once per content.
type Card struct {
	Key      string
	Kind     board.Kind
	Tag      text.Layout
	Label    text.Layout
	Lines    []text.Layout // detail, then the note
	Italic   []bool
	W, H     float32 // the box itself
	Sats     []*Sat
	TotalH   float32 // box plus satellites: what the layout places
	Question bool    // an open question: dashed, its label italic and muted
}

// Sat is a satellite: a pro, a con or an example under its box.
type Sat struct {
	ID    string
	Kind  board.Kind
	Tag   text.Layout
	Label text.Layout
	Y     float32 // top, from the card's top
	W, H  float32
}

// Rect is a satellite's box for a card drawn at b (b's height is the
// card's own).
func (s *Sat) Rect(b draw.Rect) draw.Rect {
	return draw.Rect{X: b.X + SatIndent, Y: b.Y + s.Y, W: s.W, H: s.H}
}

func style(font draw.FontID, size float32) text.Style {
	return text.Style{Font: font, Size: size}
}

func lay(m text.Measurer, s string, st text.Style, maxW float32) text.Layout {
	return m.Layout([]text.Run{{Text: s, Style: st}}, maxW)
}

// Key is everything a card's measurement depends on.
func Key(n *board.Node, sats []*board.Node) string {
	k := string(n.Kind) + "\x00" + n.Label + "\x00" + n.Detail + "\x00" + n.Note
	for _, s := range sats {
		k += "\x01" + s.ID + "\x00" + string(s.Kind) + "\x00" + s.Label + "\x00" + s.Detail
	}
	return k
}

// MeasureCard lays out a box and its satellites.
func MeasureCard(m text.Measurer, n *board.Node, sats []*board.Node) *Card {
	c := &Card{Key: Key(n, sats), Kind: n.Kind, Question: n.Kind == board.Question || n.Kind == board.Gap}
	c.Tag = lay(m, Tags[n.Kind], style(text.SansSemibold, TagSize), 0)
	labelFont := text.SansSemibold
	if c.Question {
		labelFont = text.SansItalic
	}
	c.Label = lay(m, n.Label, style(labelFont, LabelSize), CardTextW)
	for i, s := range []string{n.Detail, n.Note} {
		if s == "" {
			continue
		}
		font := text.SansRegular
		if i == 1 {
			font = text.SansItalic
		}
		c.Lines = append(c.Lines, lay(m, s, style(font, LineSize), CardTextW))
		c.Italic = append(c.Italic, i == 1)
	}
	w := max(c.Tag.Width, c.Label.Width)
	h := CardPadY + c.Tag.Height + TagGap + c.Label.Height
	for _, l := range c.Lines {
		w = max(w, l.Width)
		h += LineGap + l.Height
	}
	c.W = max(CardMinW, w+2*CardPadX)
	c.H = h + CardPadY

	y := c.H
	for _, s := range sats {
		st := &Sat{ID: s.ID, Kind: s.Kind}
		st.Tag = lay(m, Tags[s.Kind], style(text.SansSemibold, TagSize), 0)
		label := s.Label
		if s.Detail != "" {
			label += " · " + s.Detail
		}
		st.Label = lay(m, label, style(text.SansRegular, SatSize), SatTextW)
		st.W = max(st.Tag.Width, st.Label.Width) + 2*SatPadX
		st.H = SatPadY + st.Tag.Height + 3 + st.Label.Height + SatPadY
		y += SatGap
		st.Y = y
		y += st.H
		c.W = max(c.W, st.W+SatIndent)
		c.Sats = append(c.Sats, st)
	}
	c.TotalH = y
	return c
}

// Notes is the meeting's notes panel, measured.
type Notes struct {
	Key   string
	Title text.Layout
	Lines []text.Layout
	Y     []float32 // each line's top, from the panel's top
	W, H  float32
}

// NotesTitleText is the panel's heading.
const NotesTitleText = "Notes"

// MeasureNotes lays out the notes panel's lines.
func MeasureNotes(m text.Measurer, lines []string) *Notes {
	p := &Notes{}
	for _, l := range lines {
		p.Key += l + "\x00"
	}
	p.Title = lay(m, NotesTitleText, style(text.SansSemibold, NotesTitle), 0)
	w := p.Title.Width
	y := float32(NotesPad) + p.Title.Height + NotesGap + 2
	for _, l := range lines {
		tl := lay(m, l, style(text.SansRegular, NotesSize), NotesTextW)
		p.Lines = append(p.Lines, tl)
		p.Y = append(p.Y, y)
		w = max(w, NotesBullet+tl.Width)
		y += tl.Height + NotesGap
	}
	p.W = max(w, NotesTextW*0.6) + 2*NotesPad
	p.H = y - NotesGap + NotesPad
	return p
}

// Sizer sizes cards and the notes panel for the layout, keeping what it
// measured so a board laid out again measures only what changed. It is
// not safe for concurrent use.
type Sizer struct {
	M     text.Measurer // Text() when nil
	cards map[string]*Card
	notes *Notes
}

func (z *Sizer) m() text.Measurer {
	if z.M == nil {
		z.M = Text()
	}
	return z.M
}

// Measure returns the card for n, measuring it if its content changed.
func (z *Sizer) Measure(n *board.Node, sats []*board.Node) *Card {
	if z.cards == nil {
		z.cards = map[string]*Card{}
	}
	if c := z.cards[n.ID]; c != nil && c.Key == Key(n, sats) {
		return c
	}
	c := MeasureCard(z.m(), n, sats)
	z.cards[n.ID] = c
	return c
}

// MeasureNotes returns the notes panel, measuring it if it changed.
func (z *Sizer) MeasureNotes(lines []string) *Notes {
	key := ""
	for _, l := range lines {
		key += l + "\x00"
	}
	if z.notes == nil || z.notes.Key != key {
		z.notes = MeasureNotes(z.m(), lines)
	}
	return z.notes
}

// Card implements layout.Sizer.
func (z *Sizer) Card(n *board.Node, sats []*board.Node) (w, h, totalH float32) {
	c := z.Measure(n, sats)
	return c.W, c.H, c.TotalH
}

// Notes implements layout.Sizer.
func (z *Sizer) Notes(lines []string) (w, h float32) {
	p := z.MeasureNotes(lines)
	return p.W, p.H
}

// GroupLabel implements layout.Sizer.
func (z *Sizer) GroupLabel(label string) float32 {
	return lay(z.m(), label, style(text.SansSemibold, GroupLabelSize), 0).Width
}

// Place lays a board out the way the server does: measured with the
// embedded fonts, seeded from prev (nil for none).
func Place(doc *board.Doc, prev *layout.Placement) *layout.Placement {
	var z Sizer
	return layout.Board(doc, &z, prev)
}
