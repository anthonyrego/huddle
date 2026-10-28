package ui

import (
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

// BenchmarkTypeInALongPrompt types one character into a 20 KB prompt
// (60 paragraphs) and lays the field out again, as each keystroke does.
func BenchmarkTypeInALongPrompt(b *testing.B) {
	s, err := text.NewShaper()
	if err != nil {
		b.Fatal(err)
	}
	c := &Ctx{Text: s, Pal: theme.Light, Scale: 2}
	para := strings.Repeat("Route the customer's message to the one response that fits best. ", 5)
	f := NewTextField("", 8)
	f.MaxBytes = 256 << 10
	f.Mono = true
	f.SetText(strings.Repeat(para+"\n\n", 60))
	f.Focus(true)
	f.caret = len(f.text) / 2
	f.Measure(c, 300)
	for b.Loop() {
		f.Input("x")
		f.Measure(c, 300)
	}
}
