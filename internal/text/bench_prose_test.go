package text

import "testing"

// BenchmarkLayoutProse is a wrapped Inter paragraph with non-ASCII the face
// covers (the HarfBuzz path, where every rune's coverage is checked).
func BenchmarkLayoutProse(b *testing.B) {
	runs := []Run{{Text: "The renderer’s atlas is warm → every glyph is cached, so “scrolling” costs only instances – no rasterizing — and the frame time stays flat … even at 120 Hz on Retina • ✓", Style: Style{Font: SansRegular, Size: 14}}}
	s, _ := NewShaper()
	for b.Loop() {
		s.Layout(runs, 400)
	}
}
