package text

import (
	"math"
	"testing"
)

func TestEmojiSequence(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		pres presentation
	}{
		{"✅ ok", 1, presEmojiDefault},
		{"⚡", 1, presEmojiDefault},
		{"⚠", 1, presText},
		{"⚠️", 2, presEmoji},
		{"⚠︎", 2, presTextForced},
		{"✅︎", 2, presTextForced},
		{"❤️x", 2, presEmoji},
		{"©", 1, presText},
		{"©️", 2, presEmoji},
		{"1️⃣", 3, presEmoji},
		{"#⃣", 2, presEmoji},
		{"1", 0, presText},
		{"12", 0, presText},
		{"🇺🇸🇯🇵", 2, presEmoji},
		{"🇺", 1, presEmojiDefault},
		{"👍🏽", 2, presEmoji},
		{"👨‍👩‍👧 family", 5, presEmoji},
		{"🏳️‍🌈", 4, presEmoji},
		{"🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", 7, presEmoji},
		{"👍‍", 1, presEmojiDefault}, // a trailing joiner with nothing to join
		{"a", 0, presText},
		{"中", 0, presText},
		{"→", 0, presText}, // not Extended_Pictographic
		{"↔", 1, presText}, // pictographic, text by default
	} {
		n, p := emojiSequence([]rune(c.in))
		if n != c.n || p != c.pres {
			t.Errorf("%q: got (%d, %d), want (%d, %d)", c.in, n, p, c.n, c.pres)
		}
	}
}

// TestEmojiSizingMatchesCoreText checks emojiSize and emojiDrop against
// CTLineDraw measurements of 😀 in Helvetica on macOS 15: the bitmap's box
// (top and bottom relative to the baseline, y down) at each text size.
// Ink is up to 2px inside the box (the images have a transparent margin).
func TestEmojiSizingMatchesCoreText(t *testing.T) {
	for _, m := range []struct{ size, inkTop, inkBottom float32 }{
		{11, -11, 3}, {13, -13, 3}, {16, -16, 4}, {20, -19, 3}, {24, -21, 3}, {32, -27, 3}, {48, -40, 5}, {64, -54, 6},
	} {
		box := emojiSize(m.size)
		bottom := emojiDrop(m.size)
		top := bottom - box
		if top > m.inkTop+0.75 || bottom < m.inkBottom-0.75 {
			t.Errorf("%vpx: box %.2f..%.2f doesn't contain CoreText's ink %v..%v", m.size, top, bottom, m.inkTop, m.inkBottom)
		}
		if margin := float32(math.Max(float64(m.inkTop-top), float64(bottom-m.inkBottom))); margin > 0.07*box+1 {
			t.Errorf("%vpx: box %.2f..%.2f is %.1fpx larger than CoreText's ink %v..%v", m.size, top, bottom, margin, m.inkTop, m.inkBottom)
		}
	}
}
