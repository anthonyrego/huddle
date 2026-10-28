package text

import (
	"bytes"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
)

// TestMain keeps the system font index out of the real data dir.
func TestMain(m *testing.M) {
	if os.Getenv("VIEWER_DATA_DIR") == "" {
		SetFontIndexDir(filepath.Join(os.TempDir(), "huddle-text-test-fonts"))
	}
	os.Exit(m.Run())
}

func mono(size float32) Style { return Style{Font: MonoRegular, Size: size} }

// runFor returns the run of a one-line layout whose text contains s.
func runFor(t *testing.T, l Layout, s string) draw.GlyphRun {
	t.Helper()
	for _, r := range l.Lines[0].Runs {
		if strings.Contains(r.Text, s) {
			return r
		}
	}
	t.Fatalf("no run contains %q", s)
	return draw.GlyphRun{}
}

func familyOf(id draw.FontID) string {
	if f := fallbackByID(id); f != nil {
		return f.family
	}
	return "primary"
}

func needMacFonts(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the expected faces are macOS system fonts")
	}
	if info := FontIndex(); info.Err != nil || info.Fonts == 0 {
		t.Skipf("no system font index: %+v", info)
	}
}

// TestCoveredTextStaysInItsFace: text the embedded faces cover never splits
// by face, so it costs no fallback work.
func TestCoveredTextStaysInItsFace(t *testing.T) {
	s := newShaper(t)
	for _, c := range []struct {
		text string
		st   Style
	}{
		{"Café naïve “quotes” – en — em … • → ← ✓ ✗ ⚠ ★ ❤ ₿ ≠ ≤ ∞ λ Ж", sans(14)},
		{"if a → b { return x ≠ y } // ✓ ⚠ ⚡ • “q” ∀", mono(13)},
	} {
		para := []rune(c.text)
		s.fontOf = make([]draw.FontID, len(para))
		ready := false
		if s.assignFonts(para, span{0, len(para), c.st}, &ready) {
			for i, id := range s.fontOf {
				if id != c.st.Font {
					t.Errorf("%q: %q needs a fallback", c.text, para[i])
				}
			}
		}
		for _, r := range s.Layout([]Run{{Text: c.text, Style: c.st}}, 0).Lines[0].Runs {
			if r.Font != c.st.Font {
				t.Errorf("%q: run %q uses font %d", c.text, r.Text, r.Font)
			}
		}
	}
}

// TestFallbackLikeCoreText checks the face chosen for each character
// against what CTFontCreateForString picks on macOS 15 for Inter and
// JetBrains Mono (the exceptions are noted).
func TestFallbackLikeCoreText(t *testing.T) {
	needMacFonts(t)
	s := newShaper(t)
	for _, c := range []struct {
		text   string
		st     Style
		family string
	}{
		{"✅", sans(14), "applecoloremoji"},
		{"⚠️", sans(14), "applecoloremoji"},
		{"⚡", sans(14), "applecoloremoji"},
		{"⏲️", sans(14), "applecoloremoji"},
		{"✅︎", sans(14), "applecoloremoji"}, // no text face has ✅
		{"⚠", sans(14), "primary"},
		{"→", sans(14), "primary"},
		{"✓", sans(14), "primary"},
		{"❤", sans(14), "primary"},
		{"✔", sans(14), "zapfdingbats"},
		{"⏲", sans(14), "stixtwomath"},
		{"☑", sans(14), "applesymbols"},
		{"∀", sans(14), "applesymbols"},
		{"中文", sans(14), "pingfangsc"},
		{"日本語", sans(14), "pingfangsc"},    // no kana: the locale decides, like CoreText
		{"ひらがな", sans(14), "hiraginosans"}, // kana: Japanese (CoreText in English uses PingFang SC)
		{"한국어", sans(14), "applesdgothicneo"},
		{"ก", sans(14), "thonburi"},
		{"अ", sans(14), "kohinoordevanagari"},
		{"ع", sans(14), "geezapro"},
		{"⚡", mono(13), "primary"},
		{"⚠", mono(13), "primary"},
		{"⇄", mono(13), "menlo"},
		{"✔", mono(13), "menlo"},
		{"★", mono(13), "menlo"},
		{"⏲", mono(13), "stixtwomath"},
		{"✅", mono(13), "applecoloremoji"},
		{"中文", mono(13), "pingfangsc"},
		{"ก", mono(13), "ayuthaya"},
	} {
		if !strings.HasPrefix(c.family, "primary") && !familyInstalled(c.family) {
			t.Logf("%s not installed; skipping %q", c.family, c.text)
			continue
		}
		r := runFor(t, s.Layout([]Run{{Text: "a " + c.text + " b", Style: c.st}}, 0), c.text)
		if got := familyOf(r.Font); got != c.family {
			t.Errorf("%q in %s: drawn by %s, want %s", c.text, FontName(c.st.Font), got, c.family)
		}
	}
}

func familyInstalled(fam string) bool {
	if len(sysIndex.byFamily[fam]) > 0 {
		return true
	}
	extras := extraFaces()
	for i := range extras {
		if extras[i].family == fam {
			return true
		}
	}
	return false
}

// TestEmojiSequencesShapeWhole: flags, skin tones, keycaps and ZWJ
// sequences reach the emoji font as one run and come out as one glyph
// (plus zero-width selector glyphs).
func TestEmojiSequencesShapeWhole(t *testing.T) {
	needMacFonts(t)
	if !familyInstalled("applecoloremoji") {
		t.Skip("no Apple Color Emoji")
	}
	s := newShaper(t)
	for _, seq := range []string{"🇺🇸", "👍🏽", "👨‍👩‍👧", "🏳️‍🌈", "1️⃣", "❤️"} {
		r := runFor(t, s.Layout([]Run{{Text: "x " + seq + " y", Style: sans(14)}}, 0), seq)
		if familyOf(r.Font) != "applecoloremoji" || r.Text != seq {
			t.Errorf("%q: run %q in %s", seq, r.Text, FontName(r.Font))
			continue
		}
		if r.Size != emojiSize(14) || r.Glyphs[0].Y != emojiDrop(14) {
			t.Errorf("%q: size %v, y %v; want CoreText's %v, %v", seq, r.Size, r.Glyphs[0].Y, emojiSize(14), emojiDrop(14))
		}
		drawn := 0 // glyphs with a bitmap
		for _, g := range r.Glyphs {
			if _, ok := fallbackByID(r.Font).sbix.glyph(g.GID, 20); ok {
				drawn++
			}
		}
		if r.Glyphs[0].GID == 0 || drawn != 1 {
			t.Errorf("%q: glyphs %+v, want one visible glyph", seq, r.Glyphs)
		}
	}
}

// TestFallbackKeepsLineMetrics: a line with CJK, emoji and symbols is as
// tall, with the baseline in the same place, as one without, and wrapped
// lines stay evenly spaced.
func TestFallbackKeepsLineMetrics(t *testing.T) {
	s := newShaper(t)
	for _, st := range []Style{sans(14), mono(13), {Font: SansSemibold, Size: 11}} {
		plain := s.Layout([]Run{{Text: "Hxg", Style: st}}, 0).Lines[0]
		mixed := s.Layout([]Run{{Text: "Hxg 中文 ✅ ⚠️ 한 ⏲ ก 👨‍👩‍👧", Style: st}}, 0).Lines[0]
		if mixed.Height != plain.Height || mixed.Baseline != plain.Baseline {
			t.Errorf("%s %v: fallback moved the line: height %v→%v, baseline %v→%v",
				FontName(st.Font), st.Size, plain.Height, mixed.Height, plain.Baseline, mixed.Baseline)
		}
		lay := s.Layout([]Run{{Text: strings.Repeat("agents write ✅ and 中文 and 日本語の文章 ", 6), Style: st}}, 200)
		if len(lay.Lines) < 3 {
			t.Fatalf("expected wrapping, got %d lines", len(lay.Lines))
		}
		for i, l := range lay.Lines {
			if l.Height != plain.Height || math.Abs(float64(l.Baseline-l.Top-plain.Baseline)) > 0.001 {
				t.Errorf("%s: wrapped line %d height %v baseline %v, want %v %v", FontName(st.Font), i, l.Height, l.Baseline-l.Top, plain.Height, plain.Baseline)
			}
		}
	}
}

// TestMissingGlyphLoggedOnce: a character no face has is drawn as the
// primary face's .notdef and logged once per rune, not per layout.
func TestMissingGlyphLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s := newShaper(t)
	const nobody = "\U0010FFFD" // last private-use code point: no system font maps it
	for range 3 {
		l := s.Layout([]Run{{Text: "a" + nobody + "b\n" + nobody, Style: sans(14)}}, 0)
		if r := runFor(t, l, nobody); r.Font != SansRegular || r.Glyphs[len(r.Glyphs)-1].GID != 0 && r.Glyphs[0].GID != 0 {
			t.Fatalf("missing glyph run %+v, want the primary face's .notdef", r)
		}
	}
	if n := strings.Count(buf.String(), "no font has a glyph"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, buf.String())
	}
}

// TestFallbackGlyphsRasterize draws the fallback runs through the default
// rasterizer: CJK and symbols as coverage, emoji as color.
func TestFallbackGlyphsRasterize(t *testing.T) {
	needMacFonts(t)
	s := newShaper(t)
	rast, err := NewDefaultRasterizer()
	if err != nil {
		t.Fatal(err)
	}
	c := NewGlyphCache(rast)
	c.BeginFrame()
	for _, r := range s.Layout([]Run{{Text: "中 ✅ ⏲ 한", Style: sans(14)}}, 0).Lines[0].Runs {
		if r.Font < FirstFallback {
			continue
		}
		g := c.Get(r.Font, r.Glyphs[0].GID, r.Size*2, 0)
		if !g.Drawable() || g.Color != IsColorFont(r.Font) {
			t.Errorf("%q (%s): glyph %+v, color font %v", r.Text, FontName(r.Font), g, IsColorFont(r.Font))
		}
		if IsColorFont(r.Font) && (int(g.W) < int(r.Size*2)-1 || int(g.W) > int(r.Size*2)+1) {
			t.Errorf("%q: emoji bitmap %d px wide at %v px", r.Text, g.W, r.Size*2)
		}
	}
}

// TestFallbackConcurrent lays out fallback text from several shapers at
// once (prep runs off the main thread while the app draws).
func TestFallbackConcurrent(t *testing.T) {
	done := make(chan Layout)
	for range 4 {
		go func() {
			s, _ := NewShaper()
			var l Layout
			for range 20 {
				l = s.Layout([]Run{{Text: "并发 ✅ ⚠️ ⏲ 한 → ok", Style: sans(13)}}, 0)
			}
			done <- l
		}()
	}
	first := <-done
	for range 3 {
		if l := <-done; !reflect.DeepEqual(l, first) {
			t.Errorf("layouts differ between shapers:\n%+v\n%+v", l, first)
		}
	}
}
