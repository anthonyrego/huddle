package text

import (
	"slices"
	"unicode"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/harfbuzz"
)

// Splitting a span by face coverage. Every rune gets a face: the span's own
// when it has the glyph, else a fallback (fallback.go). Emoji sequences get
// one face for the whole sequence, and marks, joiners and variation
// selectors stay with the face of the character they modify.

// covers reports whether face id has a glyph for r. The embedded faces all
// cover printable ASCII.
func (s *Shaper) covers(id draw.FontID, r rune) bool {
	if r >= 0x20 && r < 0x7f && int(id) < len(s.faces) {
		return true
	}
	_, ok := s.face(id).NominalGlyph(r)
	return ok
}

// assignFonts sets s.fontOf for the span's runes and reports whether any of
// them needs a face other than the span's own; s.fontOf is only meaningful
// when it does. An ASCII span costs one pass over its runes; otherwise it is
// a cmap lookup per non-ASCII rune while the span's face covers them.
func (s *Shaper) assignFonts(para []rune, sp span, cjkReady *bool) bool {
	if s.embeddedOnly {
		return false
	}
	ascii := true
	for _, r := range para[sp.start:sp.end] {
		if r >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		// The embedded faces cover ASCII, and every emoji sequence that
		// starts with ASCII (keycaps) continues past it.
		return false
	}
	primary := sp.style.Font
	mixed := false
	cur := primary
	for i := sp.start; i < sp.end; {
		r := para[i]
		if r < 0x80 && !isKeycapBase(r) {
			s.fontOf[i], cur = primary, primary
			i++
			continue
		}
		if n, p := emojiSequence(para[i:sp.end]); n > 0 {
			id := s.emojiFont(primary, r, p, para, i, cjkReady)
			for k := i; k < i+n; k++ {
				s.fontOf[k] = id
			}
			mixed = mixed || id != primary
			cur = id
			i += n
			continue
		}
		id := primary
		switch {
		case r < 0x80:
		case harfbuzz.IsDefaultIgnorable(r) || unicode.Is(unicode.Cc, r):
			// Joiners, selectors and controls stay in the current run.
			id = cur
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) && s.covers(cur, r):
			id = cur
		case s.covers(primary, r):
		case unicode.Is(unicode.Zs, r):
			id = cur // a space the face lacks: don't switch faces for it
		default:
			id = resolveFallback(primary, r, s.cjkAt(para, i, cjkReady))
		}
		s.fontOf[i], cur = id, id
		mixed = mixed || id != primary
		i++
	}
	return mixed
}

// cjkAt is the CJK language for rune i, computed for the whole paragraph
// the first time a fallback needs it.
func (s *Shaper) cjkAt(para []rune, i int, ready *bool) cjkLang {
	if !*ready {
		s.cjk = slices.Grow(s.cjk[:0], len(para))[:len(para)]
		cjkLangs(para, s.cjk)
		*ready = true
	}
	return s.cjk[i]
}

// emojiFont picks the face for an emoji character or sequence starting at
// para[i], following macOS: an explicit emoji sequence (U+FE0F, keycap,
// flag, skin tone, ZWJ) goes to the emoji font; a character that defaults to
// emoji (✅ ⚡) keeps the span's face if that has it, else the emoji font;
// a character that defaults to text (⚠ ✔ ↔) or asks for text (U+FE0E)
// takes the span's face or the text cascade, with the emoji font last.
func (s *Shaper) emojiFont(primary draw.FontID, base rune, p presentation, para []rune, i int, cjkReady *bool) draw.FontID {
	switch p {
	case presEmoji:
		if id := resolveEmoji(primary, base); id != primary {
			return id
		}
	case presEmojiDefault:
		if s.covers(primary, base) {
			return primary
		}
		if id := resolveEmoji(primary, base); id != primary {
			return id
		}
	}
	if s.covers(primary, base) {
		return primary
	}
	return resolveFallback(primary, base, s.cjkAt(para, i, cjkReady))
}

// runFont maps a shaped output back to what its GlyphRun draws with: the
// face's FontID, the size, and how far to move glyphs down (Apple Color
// Emoji is drawn larger than the text and centered on it, like CoreText).
func (s *Shaper) runFont(face *font.Face, st Style) (id draw.FontID, size, dy float32) {
	id, ok := s.ids[face]
	if !ok {
		id = st.Font
	}
	if ff := fallbackByID(id); ff != nil && ff.appleEmoji {
		return id, emojiSize(st.Size), emojiDrop(st.Size)
	}
	return id, st.Size, 0
}
