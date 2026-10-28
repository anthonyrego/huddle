package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-text/typesetting/segmenter"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// TextField is a plain-text editor for short notes and fields: it wraps at
// its width, grows with its content and edits at a caret, with a selection
// (shift and the arrows, a drag, a double-click, cmd-A) that typing
// replaces and the owner copies (Copy). It is pure: its owner hands it
// keys, committed text and the IME's composition, and draws it.
//
// It wraps lines itself, at the Unicode line-break opportunities, measuring
// with the Measurer, so every visual line maps to a byte range of the text
// and the caret's x is the width of the line's text before it.
type TextField struct {
	Placeholder string
	MinLines    int // the field is at least this many lines tall
	MaxBytes    int // longest text it takes (0: no limit)
	Mono        bool

	text    string
	caret   int // byte offset into text
	anchor  int // the selection's other end, while selOn
	selOn   bool
	wantX   float32 // x that up and down keep to; <0 when unset
	comp    string  // the IME's composition, shown at the caret
	compCur int     // the composition's cursor, in bytes into comp
	focused bool

	// Layout cache, for one width, palette and text.
	w       float32
	pal     theme.Palette
	laidOut string // display text the lines were made from
	lines   []fieldLine
	paras   map[string][]fieldLine // each paragraph's lines, by its text, for the last layout
	lineH   float32
	style   text.Style
	m       text.Measurer
}

// fieldLine is one visual line: bytes [start, end) of the display text (a
// hard break's "\n" is in neither line) and its unwrapped layout.
type fieldLine struct {
	start, end int
	lay        text.Layout
}

// Field geometry, in logical units.
const (
	fieldPadX  = 10
	fieldPadY  = 7
	fieldSize  = 13.5
	caretWidth = 1.5
)

// FieldAction is what a key asked the field's owner to do.
type FieldAction uint8

const (
	FieldNone   FieldAction = iota
	FieldSubmit             // ⌘Enter
	FieldBlur               // Escape
)

// NewTextField returns an empty field.
func NewTextField(placeholder string, minLines int) *TextField {
	return &TextField{Placeholder: placeholder, MinLines: minLines, MaxBytes: 4000, wantX: -1}
}

// Text is the field's text.
func (f *TextField) Text() string { return f.text }

// SetText replaces the text and puts the caret at its end.
func (f *TextField) SetText(s string) {
	f.text = clean(s, f.MaxBytes)
	f.caret, f.wantX, f.comp, f.selOn = len(f.text), -1, "", false
}

// Selection returns the selected bytes [a, b) of the text; ok is false
// when nothing is selected.
func (f *TextField) Selection() (a, b int, ok bool) {
	if !f.selOn || f.anchor == f.caret {
		return 0, 0, false
	}
	return min(f.anchor, f.caret), max(f.anchor, f.caret), true
}

// SelectAll selects the whole text.
func (f *TextField) SelectAll() {
	f.anchor, f.caret, f.selOn, f.wantX = 0, len(f.text), true, -1
}

// Copy returns the selected text, and with cut removes it; ok is false
// when nothing is selected.
func (f *TextField) Copy(cut bool) (string, bool) {
	a, b, ok := f.Selection()
	if !ok {
		return "", false
	}
	s := f.text[a:b]
	if cut {
		f.deleteSelection()
	}
	return s, true
}

// deleteSelection removes the selected text, leaving the caret where it
// was; false when nothing was selected.
func (f *TextField) deleteSelection() bool {
	a, b, ok := f.Selection()
	f.selOn = false
	if !ok {
		return false
	}
	f.text = f.text[:a] + f.text[b:]
	f.caret, f.wantX = a, -1
	return true
}

// extend starts a selection at the caret unless one is under way; plain
// moves end it.
func (f *TextField) extend(shift bool) {
	switch {
	case shift && !f.selOn:
		f.selOn, f.anchor = true, f.caret
	case !shift:
		f.selOn = false
	}
}

// Focused reports whether the field has the keyboard.
func (f *TextField) Focused() bool { return f.focused }

// Focus gives the field the keyboard (or takes it away), dropping any
// composition.
func (f *TextField) Focus(on bool) {
	f.focused = on
	f.comp, f.compCur = "", 0
}

// Caret is the caret's byte offset (for tests).
func (f *TextField) Caret() int { return f.caret }

// clean normalizes line endings, turns tabs into spaces, drops other
// control characters and caps the length at max bytes (on a rune
// boundary).
func clean(s string, max int) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	if max > 0 && len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// Input inserts committed text at the caret, in place of the selection.
func (f *TextField) Input(s string) {
	f.comp, f.compCur = "", 0
	s = clean(s, 0)
	if s != "" {
		f.deleteSelection()
	}
	if f.MaxBytes > 0 && len(f.text)+len(s) > f.MaxBytes {
		s = clean(s, f.MaxBytes-len(f.text))
	}
	if s == "" {
		return
	}
	f.text = f.text[:f.caret] + s + f.text[f.caret:]
	f.caret += len(s)
	f.wantX = -1
}

// Compose shows the IME's composition at the caret; cursor is its cursor,
// in characters. An empty text ends it.
func (f *TextField) Compose(s string, cursor int) {
	f.comp = strings.ReplaceAll(s, "\n", " ")
	f.compCur = len(f.comp)
	for i := range f.comp {
		if cursor == 0 {
			f.compCur = i
			break
		}
		cursor--
	}
}

// Key handles an editing key. It reports Handled for every key without ⌘
// or Ctrl, so typing never reaches the app's single-key shortcuts; the
// text itself arrives through Input.
func (f *TextField) Key(ev Event) (Result, FieldAction) {
	if ev.Kind != KeyPress {
		return Result{Handled: ev.Kind == KeyRelease}, FieldNone
	}
	cmd := ev.Mods&(Cmd|Ctrl) != 0
	shift, alt := ev.Mods&Shift != 0, ev.Mods&Alt != 0
	if f.comp != "" {
		return Result{Handled: !cmd}, FieldNone // the IME has these keys
	}
	moved := func() (Result, FieldAction) { return Result{Handled: true, Redraw: true}, FieldNone }
	// A plain left or right on a selection goes to its edge.
	if a, b, ok := f.Selection(); ok && !shift && !cmd && !alt && (ev.Key == KeyArrowLeft || ev.Key == KeyArrowRight) {
		f.caret, f.selOn, f.wantX = a, false, -1
		if ev.Key == KeyArrowRight {
			f.caret = b
		}
		return moved()
	}
	switch ev.Key {
	case 'a':
		if cmd {
			f.SelectAll()
			return moved()
		}
	case KeyEnter:
		if cmd {
			return Result{Handled: true, Redraw: true}, FieldSubmit
		}
		f.Input("\n")
		return moved()
	case KeyEscape:
		return Result{Handled: true, Redraw: true}, FieldBlur
	case KeyTab:
		return Result{}, FieldNone
	case KeyBackspace:
		if f.deleteSelection() {
			return moved()
		}
		from := f.prevBoundary(f.caret)
		switch {
		case cmd:
			from = f.lineOf(f.caret).start
		case alt:
			from = f.prevWord(f.caret)
		}
		f.text = f.text[:from] + f.text[f.caret:]
		f.caret, f.wantX = from, -1
		return moved()
	case KeyDelete:
		if f.deleteSelection() {
			return moved()
		}
		to := f.nextBoundary(f.caret)
		if alt {
			to = f.nextWord(f.caret)
		}
		f.text = f.text[:f.caret] + f.text[to:]
		f.wantX = -1
		return moved()
	case KeyArrowLeft:
		f.extend(shift)
		switch {
		case cmd:
			f.caret = f.lineOf(f.caret).start
		case alt:
			f.caret = f.prevWord(f.caret)
		default:
			f.caret = f.prevBoundary(f.caret)
		}
		f.wantX = -1
		return moved()
	case KeyArrowRight:
		f.extend(shift)
		switch {
		case cmd:
			f.caret = f.lineOf(f.caret).end
		case alt:
			f.caret = f.nextWord(f.caret)
		default:
			f.caret = f.nextBoundary(f.caret)
		}
		f.wantX = -1
		return moved()
	case KeyHome:
		f.extend(shift)
		f.caret, f.wantX = f.lineOf(f.caret).start, -1
		return moved()
	case KeyEnd:
		f.extend(shift)
		f.caret, f.wantX = f.lineOf(f.caret).end, -1
		return moved()
	case KeyArrowUp, KeyArrowDown:
		f.extend(shift)
		switch {
		case cmd && ev.Key == KeyArrowUp:
			f.caret, f.wantX = 0, -1
		case cmd:
			f.caret, f.wantX = len(f.text), -1
		default:
			f.vertical(ev.Key == KeyArrowUp)
		}
		return moved()
	}
	return Result{Handled: !cmd}, FieldNone
}

// word reports whether r is part of a word, for word moves.
func word(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '\'' }

// prevWord is the start of the word before i (skipping what's between).
func (f *TextField) prevWord(i int) int {
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(f.text[:i])
		if word(r) {
			break
		}
		i -= n
	}
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(f.text[:i])
		if !word(r) {
			break
		}
		i -= n
	}
	return i
}

// nextWord is the end of the word after i.
func (f *TextField) nextWord(i int) int {
	for i < len(f.text) {
		r, n := utf8.DecodeRuneInString(f.text[i:])
		if word(r) {
			break
		}
		i += n
	}
	for i < len(f.text) {
		r, n := utf8.DecodeRuneInString(f.text[i:])
		if !word(r) {
			break
		}
		i += n
	}
	return i
}

// prevBoundary is the grapheme boundary before i (0 at the start).
func (f *TextField) prevBoundary(i int) int {
	prev := 0
	forGraphemes(f.text, func(start, end int) bool {
		if end >= i {
			prev = start
			return false
		}
		return true
	})
	return min(prev, i)
}

// nextBoundary is the grapheme boundary after i (len at the end).
func (f *TextField) nextBoundary(i int) int {
	next := len(f.text)
	forGraphemes(f.text, func(start, end int) bool {
		if start >= i {
			next = end
			return false
		}
		return true
	})
	return next
}

// forGraphemes calls fn with each grapheme cluster's byte range until fn
// returns false.
func forGraphemes(s string, fn func(start, end int) bool) {
	if s == "" {
		return
	}
	var seg segmenter.Segmenter
	seg.InitWithString(s)
	it := seg.GraphemeIterator()
	for it.Next() {
		g := it.Grapheme()
		if !fn(g.OffsetInBytes, g.OffsetInBytes+g.LengthInBytes) {
			return
		}
	}
}

// --- layout -----------------------------------------------------------------

// display is the text as shown: the composition inserted at the caret.
func (f *TextField) display() string {
	if f.comp == "" {
		return f.text
	}
	return f.text[:f.caret] + f.comp + f.text[f.caret:]
}

// layout wraps the display text at content width w.
func (f *TextField) layout(m text.Measurer, pal theme.Palette, w float32) {
	disp := f.display()
	if m == f.m && w == f.w && pal == f.pal && disp == f.laidOut && f.lines != nil {
		return
	}
	if m != f.m || w != f.w || pal != f.pal {
		f.paras = nil // laid out for something else
	}
	f.m, f.w, f.pal, f.laidOut = m, w, pal, disp
	f.style = text.Style{Font: text.SansRegular, Size: fieldSize, Color: pal.Text}
	if f.Mono {
		f.style.Font, f.style.Size = text.MonoRegular, fieldSize-1
	}
	f.lineH = m.Layout([]text.Run{{Text: "M", Style: f.style}}, 0).Height
	inner := max(20, w-2*fieldPadX)
	// Paragraphs wrap on their own, so a keystroke wraps again only the
	// one it changed: the others' lines come from the last layout.
	used := make(map[string][]fieldLine, len(f.paras))
	f.lines = f.lines[:0]
	for ps := 0; ; {
		pe := strings.IndexByte(disp[ps:], '\n')
		if pe < 0 {
			pe = len(disp)
		} else {
			pe += ps
		}
		para := disp[ps:pe]
		lines, ok := used[para]
		if !ok {
			if lines, ok = f.paras[para]; !ok {
				lines = f.wrap(m, para, inner)
			}
			used[para] = lines
		}
		for _, ln := range lines {
			f.lines = append(f.lines, fieldLine{start: ps + ln.start, end: ps + ln.end, lay: ln.lay})
		}
		if pe == len(disp) {
			break
		}
		ps = pe + 1
	}
	f.paras = used
}

// wrap breaks one paragraph into lines at width inner, at the Unicode
// line-break opportunities (between graphemes inside a word wider than
// the field), with offsets from the paragraph's start.
func (f *TextField) wrap(m text.Measurer, para string, inner float32) []fieldLine {
	width := func(s string) float32 {
		return m.Layout([]text.Run{{Text: s, Style: f.style}}, 0).Width
	}
	var lines []fieldLine
	emit := func(a, b int) { lines = append(lines, fieldLine{start: a, end: b}) }
	lineStart, lineW := 0, float32(0)
	if para != "" {
		var seg segmenter.Segmenter
		seg.InitWithString(para)
		it := seg.LineIterator()
		for it.Next() {
			ln := it.Line()
			a := ln.OffsetInBytes
			b := a + ln.LengthInBytes
			tw := width(strings.TrimRight(para[a:b], " "))
			if lineW > 0 && lineW+tw > inner {
				emit(lineStart, a)
				lineStart, lineW = a, 0
			}
			if tw <= inner {
				lineW += width(para[a:b])
				continue
			}
			// A word wider than the field breaks between graphemes.
			forGraphemes(para[a:b], func(gs, ge int) bool {
				if g := width(para[lineStart : a+ge]); g > inner && a+gs > lineStart {
					emit(lineStart, a+gs)
					lineStart = a + gs
				}
				return true
			})
			lineW = width(para[lineStart:b])
		}
	}
	emit(lineStart, len(para))
	for i := range lines {
		lines[i].lay = m.Layout([]text.Run{{Text: para[lines[i].start:lines[i].end], Style: f.style}}, 0)
	}
	return lines
}

// Measure lays the field out at width w and returns its height.
func (f *TextField) Measure(c *Ctx, w float32) float32 {
	f.layout(c.Text, c.Pal, w)
	return float32(max(len(f.lines), f.MinLines, 1))*f.lineH + 2*fieldPadY
}

// lineOf is the visual line holding byte offset i of the text (the later
// one where a soft wrap puts i at the end of one line and the start of
// the next). Valid after a layout without a composition.
func (f *TextField) lineOf(i int) fieldLine {
	if f.m == nil {
		return fieldLine{end: len(f.text)} // never measured: one line
	}
	if len(f.lines) == 0 || f.laidOut != f.text {
		f.layout(f.m, f.pal, f.w)
	}
	at := 0
	for k, ln := range f.lines {
		if ln.start <= i {
			at = k
		}
	}
	if at < len(f.lines) {
		return f.lines[at]
	}
	return fieldLine{}
}

func (f *TextField) lineIndex(i int) int {
	at := 0
	for k, ln := range f.lines {
		if ln.start <= i {
			at = k
		}
	}
	return at
}

// xAt is the x of display offset i on its line, from the text's left edge.
func (f *TextField) xAt(k, i int) float32 {
	ln := f.lines[k]
	if i <= ln.start {
		return 0
	}
	return f.m.Layout([]text.Run{{Text: f.laidOut[ln.start:min(i, ln.end)], Style: f.style}}, 0).Width
}

// offsetAt is the grapheme boundary on line k nearest x.
func (f *TextField) offsetAt(k int, x float32) int {
	ln := f.lines[k]
	best, bestD := ln.start, x
	if bestD < 0 {
		bestD = -x
	}
	forGraphemes(f.laidOut[ln.start:ln.end], func(_, ge int) bool {
		i := ln.start + ge
		d := f.xAt(k, i) - x
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = i, d
		}
		return true
	})
	return best
}

// vertical moves the caret a line up or down, keeping its x.
func (f *TextField) vertical(up bool) {
	if f.m == nil {
		return
	}
	f.layout(f.m, f.pal, f.w)
	k := f.lineIndex(f.caret)
	if f.wantX < 0 {
		f.wantX = f.xAt(k, f.caret)
	}
	switch {
	case up && k == 0:
		f.caret = 0
	case !up && k == len(f.lines)-1:
		f.caret = len(f.text)
	case up:
		f.caret = f.offsetAt(k-1, f.wantX)
	default:
		f.caret = f.offsetAt(k+1, f.wantX)
	}
}

// ClickAt puts the caret nearest p (field coordinates, as Draw's rect),
// where a drag (DragTo) starts selecting from.
func (f *TextField) ClickAt(p draw.Point) {
	if i, ok := f.offsetAtPoint(p); ok {
		f.caret, f.wantX = i, -1
		f.selOn, f.anchor = true, i
	}
}

// DragTo moves the caret to p, selecting from where the click was.
func (f *TextField) DragTo(p draw.Point) {
	if i, ok := f.offsetAtPoint(p); ok {
		if !f.selOn {
			f.selOn, f.anchor = true, f.caret
		}
		f.caret, f.wantX = i, -1
	}
}

// SelectWordAt selects the word at p (a double-click).
func (f *TextField) SelectWordAt(p draw.Point) {
	i, ok := f.offsetAtPoint(p)
	if !ok {
		return
	}
	a, b := i, i
	for a > 0 {
		r, n := utf8.DecodeLastRuneInString(f.text[:a])
		if !word(r) {
			break
		}
		a -= n
	}
	for b < len(f.text) {
		r, n := utf8.DecodeRuneInString(f.text[b:])
		if !word(r) {
			break
		}
		b += n
	}
	f.anchor, f.caret, f.selOn, f.wantX = a, b, true, -1
}

func (f *TextField) offsetAtPoint(p draw.Point) (int, bool) {
	if f.m == nil || len(f.lines) == 0 || f.comp != "" {
		return 0, false
	}
	f.layout(f.m, f.pal, f.w)
	k := min(max(int((p.Y-fieldPadY)/f.lineH), 0), len(f.lines)-1)
	return f.offsetAt(k, p.X-fieldPadX), true
}

// CaretRect is the caret's rect in field coordinates (for the IME's
// candidate window).
func (f *TextField) CaretRect() draw.Rect {
	if f.m == nil || len(f.lines) == 0 {
		return draw.Rect{X: fieldPadX, Y: fieldPadY, W: caretWidth, H: f.lineH}
	}
	at := f.caret + f.compCur
	k := f.lineIndex(at)
	return draw.Rect{X: fieldPadX + f.xAt(k, at), Y: fieldPadY + float32(k)*f.lineH, W: caretWidth, H: f.lineH}
}

// Draw draws the field into r (r.H from Measure).
func (f *TextField) Draw(c *Ctx, l *draw.List, r draw.Rect) {
	f.layout(c.Text, c.Pal, r.W)
	stroke := c.Pal.Border
	if f.focused {
		stroke = c.Pal.Accent
	}
	l.BorderedRect(r, c.Pal.Surface, stroke, 1, draw.Uniform(6))
	l.PushClip(r)
	defer l.PopClip()
	if f.text == "" && f.comp == "" && f.Placeholder != "" {
		ph := c.Text.Layout([]text.Run{{Text: f.Placeholder, Style: text.Style{Font: text.SansRegular, Size: fieldSize, Color: c.Pal.TextFaint}}}, r.W-2*fieldPadX)
		DrawText(l, ph, draw.Point{X: r.X + fieldPadX, Y: r.Y + fieldPadY})
	}
	sa, sb, sel := f.Selection()
	for k, ln := range f.lines {
		at := draw.Point{X: r.X + fieldPadX, Y: r.Y + fieldPadY + float32(k)*f.lineH}
		if a, b := max(sa, ln.start), min(sb, ln.end); sel && f.comp == "" && (a < b || sa <= ln.start && sb > ln.end) {
			x0, x1 := f.xAt(k, a), f.xAt(k, b)
			if sb > ln.end && x1-x0 < 4 {
				x1 = x0 + 4 // a selected line break
			}
			l.Rect(draw.Rect{X: at.X + x0, Y: at.Y, W: x1 - x0, H: f.lineH}, c.Pal.Accent.WithAlpha(0x50), draw.Radii{})
		}
		DrawText(l, ln.lay, at)
		if f.comp != "" {
			// Underline the part of the composition on this line.
			a, b := max(ln.start, f.caret), min(ln.end, f.caret+len(f.comp))
			if a < b {
				x0, x1 := f.xAt(k, a), f.xAt(k, b)
				l.Rect(draw.Rect{X: at.X + x0, Y: at.Y + f.lineH - 2, W: x1 - x0, H: 1}, c.Pal.Text, draw.Radii{})
			}
		}
	}
	if f.focused {
		cr := f.CaretRect()
		l.Rect(draw.Rect{X: r.X + cr.X, Y: r.Y + cr.Y + 1, W: cr.W, H: cr.H - 2}, c.Pal.Text, draw.Radii{})
	}
}
