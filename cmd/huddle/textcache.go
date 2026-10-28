package main

// Text helpers for the window's own chrome.
//
// Copied from an earlier project of the author's.

import (
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

// textCache shapes chrome text once per string, font and size (recolored
// at draw time).
type textCache struct {
	m map[textKey]text.Layout
}

type textKey struct {
	s    string
	font draw.FontID
	size float32
}

func (tc *textCache) lay(c *ui.Ctx, s string, font draw.FontID, size float32) text.Layout {
	if tc.m == nil || len(tc.m) > 512 {
		tc.m = map[textKey]text.Layout{}
	}
	k := textKey{s, font, size}
	if l, ok := tc.m[k]; ok {
		return l
	}
	l := c.Text.Layout([]text.Run{{Text: s, Style: text.Style{Font: font, Size: size, Color: c.Pal.Text}}}, 0)
	tc.m[k] = l
	return l
}

// drawAt draws a layout recolored with its top-left corner at x, y.
func drawAt(l *draw.List, lay text.Layout, x, y float32, col draw.Color) {
	for _, line := range lay.Lines {
		for _, run := range line.Runs {
			run.Origin.X += x
			run.Origin.Y += y
			run.Color = col
			l.Glyphs(run)
		}
	}
}

// putFitAt draws a one-line layout with its top-left corner at x, y within
// width w. When it doesn't fit, the glyphs that would cross the edge are
// dropped and "…" ends it.
func putFitAt(c *ui.Ctx, tc *textCache, l *draw.List, lay text.Layout, x, y, w float32, col draw.Color, font draw.FontID, size float32) {
	if lay.Width <= w || len(lay.Lines) == 0 {
		drawAt(l, lay, x, y, col)
		return
	}
	ell := tc.lay(c, "…", font, size)
	room := w - ell.Width
	line := lay.Lines[0]
	// Pen positions across the line's runs (fallback faces split a line
	// into several); a glyph's right edge is the next glyph's pen.
	var pens []float32
	for _, run := range line.Runs {
		for _, g := range run.Glyphs {
			pens = append(pens, run.Origin.X+g.X)
		}
	}
	keep, cut := 0, float32(0)
	for k := range pens {
		right := line.Width
		if k+1 < len(pens) {
			right = pens[k+1]
		}
		if right > room {
			break
		}
		keep, cut = k+1, right
	}
	for _, run := range line.Runs {
		if keep <= 0 {
			break
		}
		r := run
		r.Glyphs = run.Glyphs[:min(len(run.Glyphs), keep)]
		keep -= len(r.Glyphs)
		r.Origin.X += x
		r.Origin.Y += y
		r.Color = col
		l.Glyphs(r)
	}
	drawAt(l, ell, x+cut, y+(lay.Height-ell.Height)/2, col)
}
