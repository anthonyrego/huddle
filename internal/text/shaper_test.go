package text

import (
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
)

func newShaper(t *testing.T) *Shaper {
	t.Helper()
	s, err := NewShaper()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sans(size float32) Style { return Style{Font: SansRegular, Size: size, Color: draw.RGB(0x111111)} }

func TestLayoutSingleLine(t *testing.T) {
	s := newShaper(t)
	l := s.Layout([]Run{{Text: "Hello, world", Style: sans(14)}}, 0)
	if len(l.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(l.Lines))
	}
	// Inter at 14px: "Hello, world" is roughly 80px wide and 17px tall.
	if l.Width < 60 || l.Width > 110 {
		t.Errorf("width = %.1f, want about 80", l.Width)
	}
	if l.Height < 14 || l.Height > 22 {
		t.Errorf("height = %.1f, want about 17", l.Height)
	}
	run := l.Lines[0].Runs[0]
	if run.Text != "Hello, world" || len(run.Glyphs) != 12 {
		t.Errorf("run = %q with %d glyphs", run.Text, len(run.Glyphs))
	}
	if run.Origin.Y != l.Lines[0].Baseline {
		t.Errorf("run origin y %.1f != baseline %.1f", run.Origin.Y, l.Lines[0].Baseline)
	}
}

func TestLayoutWraps(t *testing.T) {
	s := newShaper(t)
	text := "the quick brown fox jumps over the lazy dog"
	one := s.Layout([]Run{{Text: text, Style: sans(14)}}, 0)
	wrapped := s.Layout([]Run{{Text: text, Style: sans(14)}}, one.Width/2)
	if len(wrapped.Lines) < 2 {
		t.Fatalf("lines = %d, want >= 2", len(wrapped.Lines))
	}
	for i, l := range wrapped.Lines {
		if l.Width > one.Width/2+0.01 {
			t.Errorf("line %d width %.1f exceeds max %.1f", i, l.Width, one.Width/2)
		}
	}
	if wrapped.Lines[1].Top <= wrapped.Lines[0].Top {
		t.Errorf("line 1 top %.1f not below line 0 top %.1f", wrapped.Lines[1].Top, wrapped.Lines[0].Top)
	}
}

func TestLayoutMixedStylesKeepColorsAndFonts(t *testing.T) {
	s := newShaper(t)
	red := draw.RGB(0xcc0000)
	runs := []Run{
		{Text: "call ", Style: sans(14)},
		{Text: "fmt.Println", Style: Style{Font: MonoRegular, Size: 13, Color: red}},
		{Text: " now", Style: sans(14)},
	}
	l := s.Layout(runs, 0)
	if len(l.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(l.Lines))
	}
	var found bool
	var prevX float32 = -1
	for _, r := range l.Lines[0].Runs {
		if r.Origin.X <= prevX {
			t.Errorf("runs not left to right: %.1f after %.1f", r.Origin.X, prevX)
		}
		prevX = r.Origin.X
		if r.Text == "fmt.Println" {
			found = true
			if r.Font != MonoRegular || r.Color != red || r.Size != 13 {
				t.Errorf("mono run style = %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("no run for the mono span: %+v", l.Lines[0].Runs)
	}
}

func TestLayoutNewlinesAndEmptyLines(t *testing.T) {
	s := newShaper(t)
	l := s.Layout([]Run{{Text: "a\n\nb", Style: sans(14)}}, 0)
	if len(l.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(l.Lines))
	}
	if l.Lines[1].Height == 0 {
		t.Error("empty line has no height")
	}
}

func TestMonoAdvancesAreUniform(t *testing.T) {
	s := newShaper(t)
	l := s.Layout([]Run{{Text: "i=>m!=w", Style: Style{Font: MonoRegular, Size: 13}}}, 0)
	g := l.Lines[0].Runs[0].Glyphs
	if len(g) != 7 {
		t.Fatalf("glyphs = %d, want 7 (ligatures must be off for code)", len(g))
	}
	step := g[1].X - g[0].X
	for i := 2; i < len(g); i++ {
		if d := g[i].X - g[i-1].X; d-step > 0.01 || step-d > 0.01 {
			t.Errorf("advance %d = %.2f, want %.2f", i, d, step)
		}
	}
}

func TestLineHeightMultiplier(t *testing.T) {
	s := newShaper(t)
	st := sans(10)
	st.LineHeight = 2
	l := s.Layout([]Run{{Text: "x\ny", Style: st}}, 0)
	if l.Height != 40 {
		t.Errorf("height = %.1f, want 40", l.Height)
	}
}

// TestEmbeddedShaperNeverFallsBack: a character the embedded faces lack
// stays in the span's face (its missing glyph) instead of a system face.
func TestEmbeddedShaperNeverFallsBack(t *testing.T) {
	s, err := NewEmbeddedShaper()
	if err != nil {
		t.Fatal(err)
	}
	l := s.Layout([]Run{{Text: "Ship 🚀 日本 now", Style: Style{Font: SansRegular, Size: 14}}}, 0)
	for _, ln := range l.Lines {
		for _, r := range ln.Runs {
			if r.Font >= FirstFallback {
				t.Fatalf("run %q uses fallback font %d", r.Text, r.Font)
			}
		}
	}
	if l.Width <= 0 {
		t.Fatal("no width")
	}
}
