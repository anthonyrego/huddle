// Package theme holds the color tokens every component draws with. Components
// never hard-code colors; they ask the active Palette. The app picks Light or
// Dark from the OS setting.
//
// Copied from an earlier project of the author's.
package theme

import "github.com/anthonyrego/huddle/internal/draw"

// Palette is the full set of color tokens.
type Palette struct {
	Dark bool

	// Surfaces, back to front.
	Background draw.Color // window background
	Surface    draw.Color // cards, panels
	SurfaceAlt draw.Color // headers, gutters, alternating rows
	Border     draw.Color // hairlines between regions
	Shadow     draw.Color // drop shadows under cards

	// Backdrop is the window's animated background: marbled color in the
	// accent's hue, darkest to lightest. Background is what's drawn when
	// the animation is off.
	Backdrop [4]draw.Color
	// Bar fills the chrome and timeline bars: Surface, translucent so the
	// background flows faintly beneath them.
	Bar draw.Color
	// Glass is the alpha surfaces take over the animated background (see
	// Translucent).
	Glass uint8

	// Text.
	Text      draw.Color
	TextMuted draw.Color
	TextFaint draw.Color // line numbers, disabled
	Link      draw.Color

	// Emphasis.
	Accent    draw.Color // focus rings, selection, the "live" marker
	Highlight draw.Color // the glow along the inside edges of a component that just changed
	// Ember is the glow just behind a burn transition's front, and
	// EmberCore its hottest, right at it. Rim glows along a diamond
	// transition's edge.
	Ember     draw.Color
	EmberCore draw.Color
	Rim       draw.Color

	// Diffs.
	AddedLine     draw.Color
	AddedWord     draw.Color
	RemovedLine   draw.Color
	RemovedWord   draw.Color
	AddedGutter   draw.Color
	RemovedGutter draw.Color

	// Callout kinds and status.
	Info    draw.Color
	Success draw.Color
	Warning draw.Color
	Error   draw.Color

	// Mark draws on the person's screen (mark_screen): vivid enough to
	// find at a glance over any app.
	Mark draw.Color

	// Diagrams.
	NodeFill   draw.Color
	NodeStroke draw.Color
	EdgeStroke draw.Color
	GroupFill  draw.Color

	// Syntax colors for code, diffs and fenced blocks. Plain text uses
	// Text; the content track maps chroma token types onto these.
	Syntax Syntax
}

// Syntax is the set of syntax-highlighting tokens.
type Syntax struct {
	Keyword     draw.Color
	Type        draw.Color
	Function    draw.Color
	String      draw.Color
	Number      draw.Color
	Constant    draw.Color // true, nil, iota, enum values
	Comment     draw.Color
	Operator    draw.Color
	Punctuation draw.Color
	Tag         draw.Color // markup tags
	Attribute   draw.Color // markup attributes, struct tags, keys
}

// Translucent returns p for drawing over the animated background: its
// surfaces (cards, panels, headers, diagram fills, diff lines) at alpha
// Glass, so the background shows a little through everything. Text,
// borders and accents are unchanged.
func (p Palette) Translucent() Palette {
	for _, c := range p.surfaces() {
		*c = c.WithAlpha(p.Glass)
	}
	return p
}

// Opaque returns p with its surfaces solid again, for what floats over
// other content (menus, dialogs, tooltips), which would show through.
func (p Palette) Opaque() Palette {
	for _, c := range p.surfaces() {
		*c = c.WithAlpha(0xff)
	}
	return p
}

// surfaces are the fills Translucent and Opaque change.
func (p *Palette) surfaces() []*draw.Color {
	return []*draw.Color{
		&p.Surface, &p.SurfaceAlt, &p.NodeFill, &p.GroupFill,
		&p.AddedLine, &p.AddedWord, &p.RemovedLine, &p.RemovedWord,
	}
}

// For returns the palette for the OS appearance.
func For(dark bool) Palette {
	if dark {
		return Dark
	}
	return Light
}

// Light is the light-mode palette.
var Light = Palette{
	Background: draw.RGB(0xf5f5f4),
	Surface:    draw.RGB(0xffffff),
	SurfaceAlt: draw.RGB(0xf0f0ef),
	Border:     draw.RGB(0xdcdcda),
	Shadow:     draw.Color{A: 0x30},

	Backdrop: [4]draw.Color{draw.RGB(0xdfe8fb), draw.RGB(0xe9effc), draw.RGB(0xf2f6fd), draw.RGB(0xfbfcff)},
	Bar:      draw.RGB(0xffffff).WithAlpha(0xd0),
	Glass:    0xcc,

	Text:      draw.RGB(0x1c1c1e),
	TextMuted: draw.RGB(0x5f5f63),
	TextFaint: draw.RGB(0x9d9da1),
	Link:      draw.RGB(0x2a64d8),

	Accent:    draw.RGB(0x2f6fed),
	Highlight: draw.RGB(0x2f6fed).WithAlpha(0x40),
	Ember:     draw.RGB(0xe0561b),
	EmberCore: draw.RGB(0xffc46b),
	Rim:       draw.RGB(0x2f6fed),

	AddedLine:     draw.RGB(0xe6f6ea),
	AddedWord:     draw.RGB(0xb4e6c1),
	RemovedLine:   draw.RGB(0xfbe9eb),
	RemovedWord:   draw.RGB(0xf4bcc3),
	AddedGutter:   draw.RGB(0x2e9d52),
	RemovedGutter: draw.RGB(0xcf3f4f),

	Info:    draw.RGB(0x2f6fed),
	Success: draw.RGB(0x2e9d52),
	Warning: draw.RGB(0xc98a09),
	Error:   draw.RGB(0xcf3f4f),

	Mark: draw.RGB(0xe8432e),

	NodeFill:   draw.RGB(0xffffff),
	NodeStroke: draw.RGB(0x8a8a90),
	EdgeStroke: draw.RGB(0x6b6b70),
	GroupFill:  draw.RGB(0xeeeeec),

	Syntax: Syntax{
		Keyword:     draw.RGB(0xcf222e),
		Type:        draw.RGB(0x953800),
		Function:    draw.RGB(0x8250df),
		String:      draw.RGB(0x0a3069),
		Number:      draw.RGB(0x0550ae),
		Constant:    draw.RGB(0x0550ae),
		Comment:     draw.RGB(0x6e7781),
		Operator:    draw.RGB(0x0550ae),
		Punctuation: draw.RGB(0x1c1c1e),
		Tag:         draw.RGB(0x116329),
		Attribute:   draw.RGB(0x0550ae),
	},
}

// Dark is the dark-mode palette.
var Dark = Palette{
	Dark: true,

	Background: draw.RGB(0x161618),
	Surface:    draw.RGB(0x1f1f22),
	SurfaceAlt: draw.RGB(0x28282c),
	Border:     draw.RGB(0x38383d),
	Shadow:     draw.Color{A: 0x70},

	Backdrop: [4]draw.Color{draw.RGB(0x0d1220), draw.RGB(0x0e1525), draw.RGB(0x1a2642), draw.RGB(0x342c4e)},
	Bar:      draw.RGB(0x1f1f22).WithAlpha(0xcb),
	Glass:    0xcc,

	Text:      draw.RGB(0xe8e8ea),
	TextMuted: draw.RGB(0xa4a4aa),
	TextFaint: draw.RGB(0x6c6c72),
	Link:      draw.RGB(0x7aa7ff),

	Accent:    draw.RGB(0x5b8def),
	Highlight: draw.RGB(0x5b8def).WithAlpha(0x4c),
	Ember:     draw.RGB(0xfeffff),
	EmberCore: draw.RGB(0x9eb0b0),
	Rim:       draw.RGB(0xfffcf3),

	AddedLine:     draw.RGB(0x16301f),
	AddedWord:     draw.RGB(0x21573a),
	RemovedLine:   draw.RGB(0x3a1a1f),
	RemovedWord:   draw.RGB(0x6b2731),
	AddedGutter:   draw.RGB(0x4cc274),
	RemovedGutter: draw.RGB(0xf06b7b),

	Info:    draw.RGB(0x5b8def),
	Success: draw.RGB(0x4cc274),
	Warning: draw.RGB(0xe0a82e),
	Error:   draw.RGB(0xf06b7b),

	Mark: draw.RGB(0xff6a4d),

	NodeFill:   draw.RGB(0x26262a),
	NodeStroke: draw.RGB(0x6e6e76),
	EdgeStroke: draw.RGB(0x9a9aa2),
	GroupFill:  draw.RGB(0x29292e),

	Syntax: Syntax{
		Keyword:     draw.RGB(0xff7b72),
		Type:        draw.RGB(0xffa657),
		Function:    draw.RGB(0xd2a8ff),
		String:      draw.RGB(0xa5d6ff),
		Number:      draw.RGB(0x79c0ff),
		Constant:    draw.RGB(0x79c0ff),
		Comment:     draw.RGB(0x8b949e),
		Operator:    draw.RGB(0x79c0ff),
		Punctuation: draw.RGB(0xe8e8ea),
		Tag:         draw.RGB(0x7ee787),
		Attribute:   draw.RGB(0x79c0ff),
	},
}
