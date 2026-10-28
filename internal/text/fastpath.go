package text

import (
	"math"
	"strings"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/font"
	"golang.org/x/image/math/fixed"
)

// The monospace ASCII fast path. Code is almost all printable ASCII in
// JetBrains Mono with ligatures and contextual alternates off, where
// HarfBuzz reduces to a cmap lookup and the glyph's advance. Doing that
// directly is several times faster, which matters when a fling brings new
// rows of a large diff into view every frame. TestFastPathMatchesHarfBuzz
// checks the two agree glyph for glyph.

const asciiFirst, asciiLast = 0x20, 0x7e

// asciiTable caches one face's glyph IDs and advances (font units) for
// printable ASCII.
type asciiTable struct {
	gid [asciiLast - asciiFirst + 1]font.GID
	adv [asciiLast - asciiFirst + 1]float32
}

func (s *Shaper) asciiFor(id draw.FontID) *asciiTable {
	if int(id) >= len(s.ascii) {
		s.ascii = append(s.ascii, make([]*asciiTable, int(id)+1-len(s.ascii))...)
	}
	if t := s.ascii[id]; t != nil {
		return t
	}
	f := s.face(id)
	t := &asciiTable{}
	for c := rune(asciiFirst); c <= asciiLast; c++ {
		g, _ := f.NominalGlyph(c)
		t.gid[c-asciiFirst] = g
		t.adv[c-asciiFirst] = f.HorizontalAdvance(g)
	}
	s.ascii[id] = t
	return t
}

// fastPathUnsafe are ASCII characters JetBrains Mono substitutes by context
// even with calt off: "`" becomes grave.case after capitals and after "`".
// Lines containing them take the HarfBuzz path.
const fastPathUnsafe = "`"

// fastPathOK reports whether every span is monospace and every rune is
// printable ASCII outside fastPathUnsafe.
func (s *Shaper) fastPathOK(para []rune, spans []span) bool {
	for _, sp := range spans {
		if !IsMono(sp.style.Font) {
			return false
		}
	}
	for _, c := range para {
		if c < asciiFirst || c > asciiLast || strings.ContainsRune(fastPathUnsafe, c) {
			return false
		}
	}
	return true
}

// asciiLine lays out one unwrapped line exactly as the HarfBuzz path would:
// go-text shapes at the size rounded up to a whole pixel and rounds each
// scaled advance to 1/64 px.
func (s *Shaper) asciiLine(para []rune, spans []span) Line {
	st := spans[0].style
	var asc, desc, gap float32
	for _, sp := range spans {
		if sp.start == sp.end {
			continue
		}
		a, d, g := s.vmetrics(sp.style)
		asc, desc, gap = max(asc, a), max(desc, d), max(gap, g)
		if sp.style.LineHeight*sp.style.Size > st.LineHeight*st.Size {
			st = sp.style
		}
	}

	var x float32
	runs := make([]draw.GlyphRun, 0, len(spans))
	for _, sp := range spans {
		if sp.start == sp.end {
			continue
		}
		t := s.asciiFor(sp.style.Font)
		scale := float32(toFixed(sp.style.Size).Ceil()<<6) / float32(s.face(sp.style.Font).Upem())
		gr := draw.GlyphRun{
			Origin: draw.Point{X: x},
			Font:   sp.style.Font,
			Size:   sp.style.Size,
			Color:  sp.style.Color,
			Glyphs: make([]draw.GlyphPos, sp.end-sp.start),
			Text:   string(para[sp.start:sp.end]),
		}
		var pen fixed.Int26_6
		for i, c := range para[sp.start:sp.end] {
			k := c - asciiFirst
			gr.Glyphs[i] = draw.GlyphPos{GID: uint32(t.gid[k]), X: fromFixed(pen)}
			pen += fixed.Int26_6(math.Round(float64(t.adv[k] * scale)))
		}
		x += fromFixed(pen)
		runs = append(runs, gr)
	}
	l := s.lineBox(st, asc, desc, gap, runs, x)
	for i := range l.Runs {
		l.Runs[i].Origin.Y = l.Baseline
	}
	return l
}
