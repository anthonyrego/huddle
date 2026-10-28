package gfx

import (
	"math"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
)

func newTextBatch(t *testing.T) (*Batch, *text.Shaper) {
	t.Helper()
	rast, err := text.NewDefaultRasterizer()
	if err != nil {
		t.Fatal(err)
	}
	s, err := text.NewShaper()
	if err != nil {
		t.Fatal(err)
	}
	b := &Batch{Glyphs: text.NewGlyphCache(rast)}
	b.Reset(800, 600)
	return b, s
}

func textList(s *text.Shaper, l *draw.List, str string, x, y float32) {
	lay := s.Layout([]text.Run{{Text: str, Style: text.Style{Font: text.SansRegular, Size: 13, Color: draw.RGB(0x202020)}}}, 0)
	for _, line := range lay.Lines {
		for _, r := range line.Runs {
			r.Origin.X += x
			r.Origin.Y += y
			l.Glyphs(r)
		}
	}
}

func TestShapesAndTextShareOneDrawCall(t *testing.T) {
	b, s := newTextBatch(t)
	var l draw.List
	l.Rect(draw.Rect{X: 10, Y: 10, W: 100, H: 20}, draw.RGB(0xffffff), draw.Radii{})
	textList(s, &l, "Hello, world", 12.3, 20)
	l.Rect(draw.Rect{X: 10, Y: 40, W: 100, H: 20}, draw.RGB(0xeeeeee), draw.Uniform(4))
	textList(s, &l, "again", 12, 50)
	b.AddList(&l, 2)
	if b.Segments() != 1 {
		t.Errorf("segments = %d, want 1: shapes don't bind textures and all glyphs share a page", b.Segments())
	}
	if b.GlyphCount != len("Hello,world")+len("again") {
		t.Errorf("glyph instances = %d", b.GlyphCount)
	}
	for _, in := range b.Instances {
		if in.Params[2] != float32(KindGlyph) {
			continue
		}
		// Whole-pixel quads: the atlas texels map 1:1 to target pixels.
		if in.Rect[0] != float32(math.Floor(float64(in.Rect[0]))) || in.Rect[1] != float32(math.Floor(float64(in.Rect[1]))) {
			t.Fatalf("glyph quad not on whole pixels: %v", in.Rect)
		}
	}
}

func TestSegmentsBreakOnScissorTransformAndTexture(t *testing.T) {
	b, s := newTextBatch(t)
	// Tiny pages so the second word spills onto a second page.
	b.Glyphs.Gray = text.NewAtlas(40, 1, 8)
	var l draw.List
	textList(s, &l, "abcdefgh", 0, 20)
	b.AddList(&l, 2)
	if len(b.Glyphs.Gray.Pages) < 2 {
		t.Fatalf("expected several pages, got %d", len(b.Glyphs.Gray.Pages))
	}
	if b.Segments() < len(b.Glyphs.Gray.Pages) {
		t.Errorf("segments = %d for %d pages: a page change must split", b.Segments(), len(b.Glyphs.Gray.Pages))
	}

	b.Reset(800, 600)
	l.Reset()
	l.Rect(draw.Rect{W: 10, H: 10}, draw.RGB(0), draw.Radii{})
	l.PushClip(draw.Rect{X: 10, Y: 10, W: 100, H: 100})
	l.Rect(draw.Rect{X: 20, Y: 20, W: 10, H: 10}, draw.RGB(0), draw.Radii{})
	l.PushClip(draw.Rect{X: 50, Y: 0, W: 100, H: 50})
	l.Rect(draw.Rect{X: 60, Y: 20, W: 10, H: 10}, draw.RGB(0), draw.Radii{})
	l.PopClip()
	l.PopClip()
	l.Rect(draw.Rect{X: 0, Y: 0, W: 10, H: 10}, draw.RGB(0), draw.Radii{})
	b.AddList(&l, 1)
	if b.Segments() != 4 {
		t.Fatalf("segments = %d, want 4 (full, clip, nested clip, full)", b.Segments())
	}
	if got := b.segs[2].scissor; got != (pxRect{50, 10, 60, 40}) {
		t.Errorf("nested scissor = %+v, want the intersection {50 10 60 40}", got)
	}

	b.SetTransform(GPUTransform{Scale: 1.5, X: 3})
	b.Add(Instance{})
	if b.Segments() != 5 {
		t.Errorf("a GPU transform change must start a segment")
	}
}

func TestColorGlyphUsesTheColorSlot(t *testing.T) {
	b, s := newTextBatch(t)
	var l draw.List
	textList(s, &l, "ab", 0, 20)
	l.Glyphs(draw.GlyphRun{Origin: draw.Point{X: 40, Y: 20}, Font: text.TestColorFont, Size: 16, Color: draw.Color{A: 255}, Glyphs: []draw.GlyphPos{{GID: 1}, {GID: 2, X: 18}}})
	textList(s, &l, "cd", 80, 20)
	b.AddList(&l, 1)
	if b.Segments() != 1 {
		t.Errorf("segments = %d: color glyphs bind their own slot and shouldn't split coverage glyphs", b.Segments())
	}
	var color int
	for _, in := range b.Instances {
		if in.Params[2] == float32(KindColorGlyph) {
			color++
		}
	}
	if color != 2 || b.segs[0].colorPage != 0 || b.segs[0].glyphPage != 0 {
		t.Errorf("color instances %d, segment %+v", color, b.segs[0].segState)
	}
}

func TestGlyphsOutsideTheClipAreNotRasterized(t *testing.T) {
	b, s := newTextBatch(t)
	var l draw.List
	l.PushClip(draw.Rect{X: 0, Y: 0, W: 60, H: 30})
	textList(s, &l, "visible then a very long tail that is scrolled out of view entirely", 0, 20)
	textList(s, &l, "a whole line below the clip", 0, 400)
	l.PopClip()
	b.AddList(&l, 1)
	if n := b.Glyphs.Stats.Rasterized; n > 16 {
		t.Errorf("rasterized %d glyphs; only those near the 60px clip should be", n)
	}
}
