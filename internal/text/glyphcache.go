package text

import (
	"math"
	"time"

	"github.com/anthonyrego/huddle/internal/draw"
)

// Glyph is a cached glyph's place in an atlas.
type Glyph struct {
	Page       int16 // atlas page; -1 means nothing to draw
	Color      bool  // in the RGBA color atlas rather than the coverage atlas
	X, Y, W, H uint16
	// Left and Top offset the bitmap from the snapped pen position (see
	// Bitmap).
	Left, Top int16
}

// Drawable reports whether the glyph has pixels.
func (g Glyph) Drawable() bool { return g.Page >= 0 }

// GlyphStats counts cache work, for the HUD and the fling report.
type GlyphStats struct {
	Rasterized      uint64        // glyphs rasterized since creation
	RasterTime      time.Duration // time spent rasterizing since creation
	Evictions       uint64        // pages cleared by LRU eviction
	FrameRasterized int           // glyphs rasterized this frame
	FrameRasterTime time.Duration
}

// GlyphCache rasterizes glyphs on demand and keeps them in two atlases:
// 8-bit coverage for outline glyphs and premultiplied RGBA for color glyphs.
// Entries are keyed by (font, glyph id, pixel size, subpixel bin).
//
// It is pure Go and single-threaded: the renderer calls it from the main
// thread while building a frame, then uploads the dirty atlas regions.
type GlyphCache struct {
	// Bins is the number of horizontal subpixel positions a glyph is
	// rasterized at: 1 snaps every pen position to whole pixels, 4 is the
	// default. Changing it requires Reset.
	Bins  int
	Gray  *Atlas
	Color *Atlas
	Stats GlyphStats

	rast  Rasterizer
	m     map[uint64]Glyph
	frame uint64
	bmp   Bitmap
}

// Default atlas geometry: an R8 page is 1 MiB and holds about two thousand
// code glyphs at 2x; the RGBA pages are 4 MiB.
const (
	DefaultPageSize      = 1024
	DefaultGrayPages     = 8
	DefaultColorPages    = 4
	DefaultSubpixelBins  = 4
	maxSubpixelBins      = 16
	maxGlyphSizePx       = 1 << 12
	glyphSizeFixedFactor = 64
)

// NewGlyphCache returns an empty cache using rast.
func NewGlyphCache(rast Rasterizer) *GlyphCache {
	c := &GlyphCache{
		Bins:  DefaultSubpixelBins,
		Gray:  NewAtlas(DefaultPageSize, 1, DefaultGrayPages),
		Color: NewAtlas(DefaultPageSize, 4, DefaultColorPages),
		rast:  rast,
		m:     map[uint64]Glyph{},
	}
	c.Gray.onEvict = c.drop
	c.Color.onEvict = c.drop
	return c
}

// SetAtlasGeometry replaces both atlases with pages of the given size and
// counts (a memory cap: gray pages cost size² bytes, color pages 4·size²).
// Every glyph is dropped.
func (c *GlyphCache) SetAtlasGeometry(pageSize, grayPages, colorPages int) {
	c.Gray = NewAtlas(pageSize, 1, grayPages)
	c.Color = NewAtlas(pageSize, 4, colorPages)
	c.Gray.onEvict = c.drop
	c.Color.onEvict = c.drop
	clear(c.m)
}

func (c *GlyphCache) drop(keys []uint64) {
	for _, k := range keys {
		delete(c.m, k)
	}
}

// BeginFrame starts a frame: pages touched from now on are protected from
// eviction until the next BeginFrame.
func (c *GlyphCache) BeginFrame() {
	c.frame++
	c.Stats.FrameRasterized = 0
	c.Stats.FrameRasterTime = 0
}

// Reset drops every glyph, for a display scale change or a new rasterizer
// setting. Atlas memory is kept.
func (c *GlyphCache) Reset() {
	clear(c.m)
	c.Gray.reset()
	c.Color.reset()
}

// Len is the number of cached glyphs.
func (c *GlyphCache) Len() int { return len(c.m) }

// Snap splits a pen x position (pixels) into a whole pixel and a subpixel
// bin, rounding to the nearest bin. With Bins == 1 it rounds to the nearest
// pixel.
func (c *GlyphCache) Snap(x float32) (px int, bin int) {
	bins := c.bins()
	n := int(math.Floor(float64(x)*float64(bins) + 0.5))
	px = floorDiv(n, bins)
	return px, n - px*bins
}

func (c *GlyphCache) bins() int {
	return min(max(c.Bins, 1), maxSubpixelBins)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func glyphKey(f draw.FontID, gid uint32, size26 uint32, bin int) uint64 {
	return uint64(f)<<52 | uint64(bin&0xf)<<48 | uint64(size26&0xffffff)<<24 | uint64(gid&0xffffff)
}

// Get returns the glyph at size px (pixels per em) and subpixel bin,
// rasterizing it on a miss.
func (c *GlyphCache) Get(f draw.FontID, gid uint32, size float32, bin int) Glyph {
	if size <= 0 || size > maxGlyphSizePx {
		return Glyph{Page: -1}
	}
	size26 := uint32(size*glyphSizeFixedFactor + 0.5)
	key := glyphKey(f, gid, size26, bin)
	if g, ok := c.m[key]; ok {
		if g.Page >= 0 {
			c.atlas(g.Color).Pages[g.Page].lastUsed = c.frame
		}
		return g
	}

	start := time.Now()
	g := Glyph{Page: -1}
	req := GlyphRequest{Font: f, GID: gid, Size: float32(size26) / glyphSizeFixedFactor, Offset: float32(bin) / float32(c.bins())}
	if c.rast.Rasterize(req, &c.bmp) && !c.bmp.Empty() {
		a := c.atlas(c.bmp.Color)
		page, x, y, evicted, ok := a.alloc(c.bmp.W, c.bmp.H, c.frame, key)
		if evicted {
			c.Stats.Evictions++
		}
		if ok {
			a.write(a.Pages[page], x, y, &c.bmp)
			g = Glyph{
				Page: int16(page), Color: c.bmp.Color,
				X: uint16(x), Y: uint16(y), W: uint16(c.bmp.W), H: uint16(c.bmp.H),
				Left: int16(c.bmp.Left), Top: int16(c.bmp.Top),
			}
		}
	}
	c.m[key] = g
	d := time.Since(start)
	c.Stats.Rasterized++
	c.Stats.RasterTime += d
	c.Stats.FrameRasterized++
	c.Stats.FrameRasterTime += d
	return g
}

func (c *GlyphCache) atlas(color bool) *Atlas {
	if color {
		return c.Color
	}
	return c.Gray
}
