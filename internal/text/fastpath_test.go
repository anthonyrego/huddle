package text

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
)

// codeLines returns printable-ASCII lines from this package's Go sources,
// plus every ASCII pair, as a corpus that exercises any kerning or
// substitution the fast path would miss.
func codeLines(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	var lines []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.ReplaceAll(l, "\t", "    ")
			if l != "" && strings.IndexFunc(l, func(r rune) bool { return r < 0x20 || r > 0x7e }) < 0 {
				lines = append(lines, l)
			}
		}
	}
	var pairs strings.Builder
	for a := rune(asciiFirst); a <= asciiLast; a++ {
		for b := rune(asciiFirst); b <= asciiLast; b++ {
			pairs.WriteRune(a)
			pairs.WriteRune(b)
		}
		lines = append(lines, pairs.String())
		pairs.Reset()
	}
	return lines
}

func TestFastPathMatchesHarfBuzz(t *testing.T) {
	fast, slow := newShaper(t), newShaper(t)
	slow.noFastPath = true
	red := draw.RGB(0xcc0000)
	lines := codeLines(t)
	// Random short strings over the whole range, for longer contexts.
	rng := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		b := make([]byte, 2+rng.IntN(7))
		for i := range b {
			b[i] = byte(asciiFirst + rng.IntN(asciiLast-asciiFirst+1))
		}
		lines = append(lines, string(b))
	}
	if len(lines) < 200 {
		t.Fatalf("corpus too small: %d lines", len(lines))
	}
	for _, size := range []float32{12, 13, 24, 26} {
		for _, f := range []draw.FontID{MonoRegular, MonoBold} {
			for i, l := range lines {
				// Split some lines into styled spans, as syntax colors do.
				runs := []Run{{Text: l, Style: Style{Font: f, Size: size}}}
				if i%3 == 0 && len(l) > 6 {
					runs = []Run{
						{Text: l[:3], Style: Style{Font: MonoBold, Size: size, Color: red}},
						{Text: l[3:], Style: Style{Font: f, Size: size}},
					}
				}
				a, b := fast.Layout(runs, 0), slow.Layout(runs, 0)
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("size %v font %d line %q:\nfast %+v\nslow %+v", size, f, l, a, b)
				}
			}
		}
	}
}

func BenchmarkLayoutCodeLine(b *testing.B) {
	line := `    if err := r.renderer.Render(cmd, sc.Texture, a.window.SwapchainFormat(), pw, ph, a.pal.Background, &a.batch); err != nil {`
	runs := []Run{{Text: line, Style: Style{Font: MonoRegular, Size: 12}}}
	for _, mode := range []struct {
		name string
		slow bool
	}{{"fast", false}, {"harfbuzz", true}} {
		b.Run(mode.name, func(b *testing.B) {
			s, _ := NewShaper()
			s.noFastPath = mode.slow
			for b.Loop() {
				s.Layout(runs, 0)
			}
		})
	}
}
