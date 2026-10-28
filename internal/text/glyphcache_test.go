package text

import (
	"testing"
)

func gid(t *testing.T, s *Shaper, f int, r rune) uint32 {
	t.Helper()
	g, ok := s.faces[f].NominalGlyph(r)
	if !ok {
		t.Fatalf("no glyph for %q", r)
	}
	return uint32(g)
}

func TestVectorRasterizerDrawsAGlyph(t *testing.T) {
	s := newShaper(t)
	r, err := NewVectorRasterizer()
	if err != nil {
		t.Fatal(err)
	}
	var b Bitmap
	if !r.Rasterize(GlyphRequest{Font: SansRegular, GID: gid(t, s, int(SansRegular), 'H'), Size: 32}, &b) {
		t.Fatal("rasterize failed")
	}
	// Inter's cap height is about 0.73 em: 23 px at 32 px/em.
	if b.H < 22 || b.H > 26 || b.W < 16 || b.W > 26 {
		t.Errorf("H bitmap is %d×%d, want about 20×24", b.W, b.H)
	}
	if b.Top > -22 || b.Top < -26 {
		t.Errorf("top = %d, want about -24 (above the baseline)", b.Top)
	}
	var full, empty int
	for _, c := range b.Pix {
		switch c {
		case 255:
			full++
		case 0:
			empty++
		}
	}
	if full < 50 || empty < 50 {
		t.Errorf("coverage looks wrong: %d full, %d empty of %d", full, empty, len(b.Pix))
	}

	var space Bitmap
	if !r.Rasterize(GlyphRequest{Font: SansRegular, GID: gid(t, s, int(SansRegular), ' '), Size: 32}, &space) || !space.Empty() {
		t.Errorf("space should rasterize to an empty bitmap, got %d×%d", space.W, space.H)
	}
}

func ink(b *Bitmap) (sum int) {
	for _, c := range b.Pix {
		sum += int(c)
	}
	return sum
}

func TestSubpixelOffsetShiftsCoverage(t *testing.T) {
	s := newShaper(t)
	r, _ := NewVectorRasterizer()
	g := gid(t, s, int(MonoRegular), 'l')
	var a, half, whole Bitmap
	r.Rasterize(GlyphRequest{Font: MonoRegular, GID: g, Size: 16, Offset: 0}, &a)
	r.Rasterize(GlyphRequest{Font: MonoRegular, GID: g, Size: 16, Offset: 0.5}, &half)
	r.Rasterize(GlyphRequest{Font: MonoRegular, GID: g, Size: 16, Offset: 1}, &whole)
	// A whole-pixel offset is the same bitmap one pixel to the right.
	if whole.Left != a.Left+1 || whole.W != a.W || string(whole.Pix) != string(a.Pix) {
		t.Errorf("offset 1 is not offset 0 moved by a pixel: left %d vs %d", whole.Left, a.Left)
	}
	// A half-pixel offset changes the bitmap but conserves the ink.
	if string(half.Pix) == string(a.Pix) {
		t.Error("offset 0.5 produced the same coverage as offset 0")
	}
	if d := ink(&half) - ink(&a); d < -ink(&a)/100 || d > ink(&a)/100 {
		t.Errorf("ink changed by %d of %d between offsets", d, ink(&a))
	}
}

func TestGlyphCacheHitsAndBins(t *testing.T) {
	s := newShaper(t)
	rast, err := NewDefaultRasterizer()
	if err != nil {
		t.Fatal(err)
	}
	c := NewGlyphCache(rast)
	c.BeginFrame()
	g := gid(t, s, int(MonoRegular), 'x')
	a := c.Get(MonoRegular, g, 24, 0)
	b := c.Get(MonoRegular, g, 24, 0)
	if a != b || c.Stats.Rasterized != 1 {
		t.Fatalf("second lookup rasterized again (%d rasterizations)", c.Stats.Rasterized)
	}
	if !a.Drawable() || a.Color {
		t.Fatalf("x should be a coverage glyph: %+v", a)
	}
	c.Get(MonoRegular, g, 24, 2)
	c.Get(MonoRegular, g, 24.5, 0)
	c.Get(MonoBold, g, 24, 0)
	if c.Len() != 4 || c.Stats.Rasterized != 4 {
		t.Errorf("bins, sizes and fonts must be separate entries: %d entries, %d rasterized", c.Len(), c.Stats.Rasterized)
	}
	// The atlas holds the bitmap where the entry says.
	p := c.Gray.Pages[a.Page]
	var ink int
	for y := int(a.Y); y < int(a.Y+a.H); y++ {
		for x := int(a.X); x < int(a.X+a.W); x++ {
			ink += int(p.Pix[y*c.Gray.PageSize+x])
		}
	}
	if ink == 0 {
		t.Error("atlas region for x is empty")
	}
}

func TestColorGlyphGoesToTheRGBAAtlas(t *testing.T) {
	rast, _ := NewDefaultRasterizer()
	c := NewGlyphCache(rast)
	c.BeginFrame()
	g := c.Get(TestColorFont, 3, 20, 0)
	if !g.Drawable() || !g.Color {
		t.Fatalf("synthetic color glyph: %+v", g)
	}
	if len(c.Color.Pages) != 1 || len(c.Gray.Pages) != 0 {
		t.Fatalf("pages: color %d gray %d, want 1 and 0", len(c.Color.Pages), len(c.Gray.Pages))
	}
	p := c.Color.Pages[0].Pix
	stride := c.Color.PageSize * 4
	cx, cy := int(g.X+g.W/2), int(g.Y+g.H/2)
	i := cy*stride + cx*4
	if p[i+3] != 255 {
		t.Errorf("center alpha = %d, want opaque", p[i+3])
	}
	for ch := range 3 {
		if p[i+ch] > p[i+3] {
			t.Errorf("channel %d = %d exceeds alpha %d: not premultiplied", ch, p[i+ch], p[i+3])
		}
	}
}

func TestSnap(t *testing.T) {
	c := &GlyphCache{Bins: 4}
	for _, tc := range []struct {
		x       float32
		px, bin int
	}{
		{10, 10, 0}, {10.3, 10, 1}, {10.5, 10, 2}, {10.9, 11, 0}, {-0.1, 0, 0}, {-0.3, -1, 3},
	} {
		if px, bin := c.Snap(tc.x); px != tc.px || bin != tc.bin {
			t.Errorf("Snap(%v) = %d,%d want %d,%d", tc.x, px, bin, tc.px, tc.bin)
		}
	}
	c.Bins = 1
	if px, bin := c.Snap(10.5); px != 11 || bin != 0 {
		t.Errorf("snap mode: Snap(10.5) = %d,%d want 11,0", px, bin)
	}
}

// fixedRast returns w×h bitmaps of a constant value, for packing tests.
type fixedRast struct{ w, h int }

func (f fixedRast) Rasterize(req GlyphRequest, dst *Bitmap) bool {
	dst.W, dst.H, dst.Left, dst.Top, dst.Color = f.w, f.h, 0, -f.h, false
	dst.Pix = make([]byte, f.w*f.h)
	for i := range dst.Pix {
		dst.Pix[i] = byte(req.GID)
	}
	return true
}

func TestAtlasEvictsLeastRecentlyUsedPage(t *testing.T) {
	c := NewGlyphCache(fixedRast{w: 7, h: 7})
	c.Gray = NewAtlas(32, 1, 2) // 4×4 cells of 8×8 per page: 16 glyphs
	c.Gray.onEvict = c.drop

	c.BeginFrame() // frame 1: fill page 0
	for g := range 16 {
		c.Get(0, uint32(g+1), 10, 0)
	}
	c.BeginFrame() // frame 2: fill page 1
	for g := range 16 {
		c.Get(0, uint32(g+100), 10, 0)
	}
	if len(c.Gray.Pages) != 2 || c.Stats.Evictions != 0 {
		t.Fatalf("pages %d evictions %d, want 2 and 0", len(c.Gray.Pages), c.Stats.Evictions)
	}
	c.BeginFrame() // frame 3: touch page 1 so page 0 is the LRU
	c.Get(0, 100, 10, 0)
	g := c.Get(0, 500, 10, 0)
	if c.Stats.Evictions != 1 || g.Page != 0 {
		t.Fatalf("new glyph landed on page %d after %d evictions; want page 0 evicted", g.Page, c.Stats.Evictions)
	}
	if c.Len() != 17 {
		t.Errorf("entries = %d, want 17 (page 0's 16 dropped, 1 added)", c.Len())
	}
	before := c.Stats.Rasterized
	c.Get(0, 1, 10, 0) // was on the evicted page: must rasterize again
	if c.Stats.Rasterized != before+1 {
		t.Error("evicted glyph was not rasterized again")
	}

	// Everything drawn this frame: the atlas grows rather than evict it.
	c.BeginFrame()
	for g := range 40 {
		c.Get(0, uint32(g+1000), 10, 0)
	}
	if len(c.Gray.Pages) < 3 {
		t.Errorf("pages = %d; a frame that needs more than MaxPages must grow the atlas", len(c.Gray.Pages))
	}
}

func TestAtlasDirtyRegionsCoverNewGlyphs(t *testing.T) {
	c := NewGlyphCache(fixedRast{w: 5, h: 9})
	c.Gray = NewAtlas(64, 1, 1)
	c.BeginFrame()
	var gs []Glyph
	for g := range 5 {
		gs = append(gs, c.Get(0, uint32(g+1), 10, 0))
	}
	var regions []Region
	c.Gray.Pages[0].Dirty(func(r Region) { regions = append(regions, r) })
	c.Gray.Pages[0].ClearDirty()
	if len(regions) != 1 {
		t.Fatalf("regions = %v, want one shelf span", regions)
	}
	r := regions[0]
	if r.X != 0 || r.W != 64 {
		t.Errorf("a new shelf should upload its full width, got %+v", r)
	}
	for _, g := range gs {
		if int(g.X) < r.X || int(g.X+g.W) > r.X+r.W || int(g.Y) < r.Y || int(g.Y+g.H) > r.Y+r.H {
			t.Errorf("glyph %+v outside dirty region %+v", g, r)
		}
	}
	c.Gray.Pages[0].Dirty(func(r Region) { t.Errorf("page still dirty: %+v", r) })
	c.Get(0, 99, 10, 0)
	var again []Region
	c.Gray.Pages[0].Dirty(func(r Region) { again = append(again, r) })
	if len(again) != 1 || again[0].W != 5+gutter {
		t.Errorf("after one more glyph, dirty = %v, want one cell", again)
	}
}
