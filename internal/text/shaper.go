package text

import (
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/math/fixed"
)

// Shaper is the go-text implementation of Measurer. It is safe for
// concurrent use; calls are serialized because go-text faces and the
// HarfBuzz shaper keep internal caches.
type Shaper struct {
	mu      sync.Mutex
	faces   []*font.Face
	hb      shaping.HarfbuzzShaper
	wrapper shaping.LineWrapper
	seg     shaping.Segmenter
	runes   []rune

	// Fallback faces this shaper has used (its own *font.Face each: they
	// are not safe to share), and the FontID behind every face.
	fallback map[draw.FontID]*font.Face
	ids      map[*font.Face]draw.FontID
	// fontOf is the face chosen for each rune of the paragraph being laid
	// out.
	fontOf []draw.FontID
	cjk    []cjkLang // CJK language per rune, computed when a fallback needs it

	// ascii caches cmap and advance lookups for the monospace fast path,
	// per face. noFastPath forces HarfBuzz (tests compare the two).
	ascii      []*asciiTable
	noFastPath bool
}

// NewShaper returns a Shaper over the embedded fonts, falling back to
// system fonts (see fallback.go) for characters they lack.
func NewShaper() (*Shaper, error) {
	fs, err := embeddedFonts()
	if err != nil {
		return nil, err
	}
	s := &Shaper{fallback: map[draw.FontID]*font.Face{}, ids: map[*font.Face]draw.FontID{}}
	for i, f := range fs {
		face := font.NewFace(f)
		s.faces = append(s.faces, face)
		s.ids[face] = draw.FontID(i)
	}
	return s, nil
}

var monoFeatures = []shaping.FontFeature{
	{Tag: ot.MustNewTag("liga"), Value: 0},
	{Tag: ot.MustNewTag("calt"), Value: 0},
}

type singleFace struct{ f *font.Face }

func (s singleFace) ResolveFace(rune) *font.Face { return s.f }

func toFixed(v float32) fixed.Int26_6 { return fixed.Int26_6(math.Round(float64(v) * 64)) }

func fromFixed(v fixed.Int26_6) float32 { return float32(v) / 64 }

// span maps a rune range of the paragraph back to the Run it came from.
type span struct {
	start, end int
	style      Style
}

// Layout implements Measurer.
func (s *Shaper) Layout(runs []Run, maxWidth float32) Layout {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out Layout
	var y float32
	for _, para := range splitParagraphs(runs) {
		lines := s.layoutParagraph(para, maxWidth)
		for _, l := range lines {
			l.Top += y
			l.Baseline += y
			for i := range l.Runs {
				l.Runs[i].Origin.Y += y
			}
			out.Lines = append(out.Lines, l)
			out.Width = max(out.Width, l.Width)
		}
		for _, l := range lines {
			y += l.Height
		}
	}
	out.Height = y
	return out
}

// splitParagraphs breaks runs at "\n" into paragraphs of runs. An empty
// paragraph keeps one empty run so it still produces a line of the right height.
// breaks maps the other paragraph and line terminators to "\n". HarfBuzz
// segmentation treats them as mandatory breaks inside what we shape as one
// paragraph, which crashed the wrapper; agents paste terminal output with
// \r\n often enough that this has to hold for every caller.
var breaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

func splitParagraphs(runs []Run) [][]Run {
	var paras [][]Run
	var cur []Run
	var last Style
	for _, r := range runs {
		last = r.Style
		text := r.Text
		if strings.ContainsAny(text, "\r\u0085\u2028\u2029") {
			text = breaks.Replace(text)
		}
		for {
			i := indexNewline(text)
			if i < 0 {
				if text != "" {
					cur = append(cur, Run{Text: text, Style: r.Style})
				}
				break
			}
			if i > 0 {
				cur = append(cur, Run{Text: text[:i], Style: r.Style})
			}
			if len(cur) == 0 {
				cur = []Run{{Style: r.Style}}
			}
			paras = append(paras, cur)
			cur = nil
			text = text[i+1:]
		}
	}
	if len(cur) > 0 || len(paras) == 0 {
		if len(cur) == 0 {
			cur = []Run{{Style: last}}
		}
		paras = append(paras, cur)
	}
	return paras
}

func indexNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

func (s *Shaper) face(id draw.FontID) *font.Face {
	if int(id) < len(s.faces) {
		return s.faces[id]
	}
	if f := s.fallback[id]; f != nil {
		return f
	}
	if ff := fallbackByID(id); ff != nil {
		f := font.NewFace(ff.font)
		s.fallback[id], s.ids[f] = f, id
		return f
	}
	return s.faces[SansRegular]
}

func (s *Shaper) layoutParagraph(runs []Run, maxWidth float32) []Line {
	s.runes = s.runes[:0]
	spans := make([]span, 0, len(runs))
	for _, r := range runs {
		start := len(s.runes)
		for _, c := range r.Text {
			s.runes = append(s.runes, c)
		}
		spans = append(spans, span{start: start, end: len(s.runes), style: r.Style})
	}
	para := s.runes

	if len(para) == 0 {
		st := runs[0].Style
		asc, desc, gap := s.vmetrics(st)
		return []Line{s.lineBox(st, asc, desc, gap, nil, 0)}
	}

	if maxWidth <= 0 && !s.noFastPath && s.fastPathOK(para, spans) {
		return []Line{s.asciiLine(para, spans)}
	}

	var outputs []shaping.Output
	s.fontOf = slices.Grow(s.fontOf[:0], len(para))[:len(para)]
	cjkReady := false
	for _, sp := range spans {
		if sp.start == sp.end {
			continue
		}
		mono := IsMono(sp.style.Font)
		mixed := s.assignFonts(para, sp, &cjkReady)
		in := shaping.Input{
			Text:      para,
			RunStart:  sp.start,
			RunEnd:    sp.end,
			Direction: di.DirectionLTR,
			Face:      s.face(sp.style.Font),
			Size:      toFixed(sp.style.Size),
			Script:    language.Latin,
			Language:  language.NewLanguage("en"),
		}
		if mono {
			in.FontFeatures = monoFeatures
		}
		for _, part := range s.seg.Split(in, singleFace{in.Face}) {
			if !mixed {
				if mono {
					part.FontFeatures = monoFeatures
				}
				outputs = append(outputs, s.hb.Shape(part))
				continue
			}
			// Shape each stretch of runes that shares a face on its own.
			for a := part.RunStart; a < part.RunEnd; {
				id := s.fontOf[a]
				b := a + 1
				for b < part.RunEnd && s.fontOf[b] == id {
					b++
				}
				sub := part
				sub.RunStart, sub.RunEnd = a, b
				sub.Face = s.face(id)
				sub.FontFeatures = nil
				if ff := fallbackByID(id); ff != nil && ff.appleEmoji {
					sub.Size = toFixed(emojiSize(sp.style.Size))
				} else if mono {
					sub.FontFeatures = monoFeatures
				}
				outputs = append(outputs, s.hb.Shape(sub))
				a = b
			}
		}
	}

	var wrapped []shaping.Line
	if maxWidth > 0 {
		wrapped, _ = s.wrapper.WrapParagraphF(
			shaping.WrapConfig{Direction: di.DirectionLTR},
			toFixed(maxWidth), para, shaping.NewSliceIterator(outputs),
		)
	} else {
		wrapped = []shaping.Line{outputs}
	}

	lines := make([]Line, 0, len(wrapped))
	var y float32
	for _, wl := range wrapped {
		sort.Slice(wl, func(i, j int) bool { return wl[i].VisualIndex < wl[j].VisualIndex })
		var asc, desc, gap float32
		st := styleAt(spans, wl[0].Runes.Offset)
		for _, o := range wl {
			rs := styleAt(spans, o.Runes.Offset)
			a, d, g := s.vmetrics(rs)
			asc, desc, gap = max(asc, a), max(desc, d), max(gap, g)
			if rs.LineHeight*rs.Size > st.LineHeight*st.Size {
				st = rs
			}
		}
		var x float32
		glyphRuns := make([]draw.GlyphRun, 0, len(wl))
		for _, o := range wl {
			rs := styleAt(spans, o.Runes.Offset)
			id, size, dy := s.runFont(o.Face, rs)
			gr := draw.GlyphRun{
				Origin: draw.Point{X: x},
				Font:   id,
				Size:   size,
				Color:  rs.Color,
				Glyphs: make([]draw.GlyphPos, len(o.Glyphs)),
				Text:   string(para[o.Runes.Offset : o.Runes.Offset+o.Runes.Count]),
			}
			var pen fixed.Int26_6
			for i, g := range o.Glyphs {
				gr.Glyphs[i] = draw.GlyphPos{
					GID: uint32(g.GlyphID),
					X:   fromFixed(pen + g.XOffset),
					Y:   dy - fromFixed(g.YOffset),
				}
				pen += g.Advance
			}
			x += fromFixed(o.Advance)
			glyphRuns = append(glyphRuns, gr)
		}
		l := s.lineBox(st, asc, desc, gap, glyphRuns, x)
		l.Top += y
		l.Baseline += y
		for i := range l.Runs {
			l.Runs[i].Origin.Y = l.Baseline
		}
		y += l.Height
		lines = append(lines, l)
	}
	return lines
}

// lineBox sizes a line from its tallest metrics. With an explicit
// LineHeight the extra leading is split above and below, like CSS.
func (s *Shaper) lineBox(st Style, asc, desc, gap float32, runs []draw.GlyphRun, width float32) Line {
	content := asc + desc
	height := content + gap
	top := gap / 2
	if st.LineHeight > 0 {
		height = st.LineHeight * st.Size
		top = (height - content) / 2
	}
	return Line{Top: 0, Baseline: top + asc, Height: height, Width: width, Runs: runs}
}

// vmetrics returns ascent, descent (positive, below the baseline) and line
// gap for a style, in logical px.
func (s *Shaper) vmetrics(st Style) (asc, desc, gap float32) {
	f := s.face(st.Font)
	ext, _ := f.FontHExtents()
	scale := st.Size / float32(f.Upem())
	return ext.Ascender * scale, -ext.Descender * scale, ext.LineGap * scale
}

func styleAt(spans []span, runeOffset int) Style {
	for _, sp := range spans {
		if runeOffset >= sp.start && runeOffset < sp.end {
			return sp.style
		}
	}
	return spans[len(spans)-1].style
}
