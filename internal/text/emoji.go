package text

import "sort"

// Emoji sequences and presentation (UTS #51). The shaper picks one face per
// sequence, so a keycap, flag, skin tone or ZWJ family is shaped whole by the
// emoji font, whose GSUB or morx turns it into one glyph.

// presentation is how a character or sequence asks to be drawn.
type presentation uint8

const (
	presText         presentation = iota // text by default (Emoji_Presentation=No)
	presEmojiDefault                     // Emoji_Presentation=Yes, no selector
	presEmoji                            // explicitly emoji: U+FE0F, keycap, flag, modifier or ZWJ
	presTextForced                       // explicitly text: U+FE0E
)

const (
	zwj      = 0x200D
	vs15     = 0xFE0E // text presentation selector
	vs16     = 0xFE0F // emoji presentation selector
	keycap   = 0x20E3 // combining enclosing keycap
	riFirst  = 0x1F1E6
	riLast   = 0x1F1FF
	modFirst = 0x1F3FB // skin tone modifiers
	modLast  = 0x1F3FF
	tagFirst = 0xE0020 // tag sequences (subdivision flags)
	tagLast  = 0xE007F
)

func inRanges(t [][2]rune, r rune) bool {
	i := sort.Search(len(t), func(i int) bool { return t[i][1] >= r })
	return i < len(t) && t[i][0] <= r
}

func isEmojiPresentation(r rune) bool { return r >= 0x231A && inRanges(emojiPresentation, r) }

func isPictographic(r rune) bool { return r >= 0xA9 && inRanges(extendedPictographic, r) }

func isRegionalIndicator(r rune) bool { return r >= riFirst && r <= riLast }

func isKeycapBase(r rune) bool { return r >= '0' && r <= '9' || r == '#' || r == '*' }

// emojiSequence reports whether text starts with an emoji character or
// sequence, and if so its length in runes and how it asks to be presented.
// A lone pictograph with text presentation (⚠, ↔) is still a sequence of
// one: the caller decides its face from the presentation.
func emojiSequence(text []rune) (n int, p presentation) {
	if len(text) == 0 {
		return 0, presText
	}
	r := text[0]
	switch {
	case isRegionalIndicator(r):
		if len(text) > 1 && isRegionalIndicator(text[1]) {
			return 2, presEmoji
		}
		return 1, presEmojiDefault
	case isKeycapBase(r):
		// 1️⃣ is 1 FE0F 20E3; the selector is sometimes left out.
		if len(text) > 2 && text[1] == vs16 && text[2] == keycap {
			return 3, presEmoji
		}
		if len(text) > 1 && text[1] == keycap {
			return 2, presEmoji
		}
		return 0, presText
	case !isPictographic(r):
		return 0, presText
	}

	p = presText
	if isEmojiPresentation(r) {
		p = presEmojiDefault
	}
	n = 1
	for n < len(text) {
		c := text[n]
		switch {
		case c == vs16:
			p = presEmoji
		case c == vs15:
			if p != presEmoji {
				p = presTextForced
			}
		case c >= modFirst && c <= modLast, c >= tagFirst && c <= tagLast, c == keycap:
			p = presEmoji
		case c == zwj && n+1 < len(text) && isPictographic(text[n+1]):
			p = presEmoji
			n++ // the joined pictograph
		default:
			return n, p
		}
		n++
	}
	return n, p
}

// Apple Color Emoji follows CoreText's sizing, measured with CTLineDraw on
// macOS 15: emoji are drawn larger than the text at small sizes (1.25× up to
// 16px, easing to 1× at 24px) and centered 0.375em above the baseline, so a
// 13px line gets a 16px emoji that dips 3px below the baseline, as in every
// native app.

// emojiSize is the size Apple Color Emoji is drawn at within text of size s.
func emojiSize(s float32) float32 {
	switch {
	case s <= 16:
		return s * 1.25
	case s < 24:
		return 20 + (s-16)*0.5
	}
	return s
}

// emojiDrop is how far below the baseline the emoji's box (emojiSize square,
// bottom on the baseline in the font's own terms) is moved, in the text's
// units.
func emojiDrop(s float32) float32 { return emojiSize(s)/2 - 0.375*s }
