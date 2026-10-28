package text

import (
	"image"
	imgdraw "image/draw"
	"math"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"golang.org/x/image/vector"
)

// Bitmap is one rasterized glyph on the CPU. internal/gfx copies it into a
// GPU atlas page; nothing here touches the GPU.
type Bitmap struct {
	W, H int
	// Left and Top place the bitmap's top-left pixel relative to the pen
	// position on the baseline, in whole pixels, y down (Top is usually
	// negative: the glyph sits above the baseline).
	Left, Top int
	// Color marks premultiplied RGBA pixels (4 bytes each) for the color
	// atlas; otherwise Pix is 8-bit coverage.
	Color bool
	Pix   []byte
}

// Empty reports whether there is nothing to draw (a space, say).
func (b *Bitmap) Empty() bool { return b.W <= 0 || b.H <= 0 }

// GlyphRequest is what to rasterize.
type GlyphRequest struct {
	Font draw.FontID
	GID  uint32
	// Size is pixels per em: the run's logical size times the display scale.
	Size float32
	// Offset is the horizontal subpixel position of the pen, in [0, 1) px.
	Offset float32
}

// Rasterizer turns glyphs into bitmaps. The default is VectorRasterizer
// (x/image/vector over go-text outlines). Implementations are used from one
// goroutine at a time and reuse dst.Pix.
type Rasterizer interface {
	// Rasterize draws req into dst. It returns false if the font has no
	// drawable data for the glyph; an empty glyph (space) returns true with
	// an empty bitmap.
	Rasterize(req GlyphRequest, dst *Bitmap) bool
}

// VectorRasterizer rasterizes TrueType and CFF outlines with
// golang.org/x/image/vector. It does no hinting: the analytic coverage is
// exact for the requested subpixel offset, and crispness at 1x comes from
// baseline snapping, subpixel bins and the shader's gamma and contrast.
type VectorRasterizer struct {
	faces    []*font.Face // own faces: *font.Face is not safe to share
	fallback map[draw.FontID]*font.Face
	z        vector.Rasterizer
	alpha    image.Alpha
}

// NewVectorRasterizer returns a rasterizer over the embedded fonts and any
// fallback faces the shaper has chosen.
func NewVectorRasterizer() (*VectorRasterizer, error) {
	fs, err := embeddedFonts()
	if err != nil {
		return nil, err
	}
	r := &VectorRasterizer{fallback: map[draw.FontID]*font.Face{}}
	for _, f := range fs {
		r.faces = append(r.faces, font.NewFace(f))
	}
	return r, nil
}

// face returns the rasterizer's own face for an embedded or fallback font,
// or nil.
func (r *VectorRasterizer) face(id draw.FontID) *font.Face {
	if int(id) < len(r.faces) {
		return r.faces[id]
	}
	if f := r.fallback[id]; f != nil {
		return f
	}
	ff := fallbackByID(id)
	if ff == nil {
		return nil
	}
	f := font.NewFace(ff.font)
	r.fallback[id] = f
	return f
}

// Rasterize implements Rasterizer.
func (r *VectorRasterizer) Rasterize(req GlyphRequest, dst *Bitmap) bool {
	*dst = Bitmap{Pix: dst.Pix[:0]}
	face := r.face(req.Font)
	if face == nil {
		return false
	}
	outline, ok := face.GlyphDataOutline(font.GID(req.GID))
	if !ok {
		return false
	}
	if len(outline.Segments) == 0 {
		return true
	}
	s := req.Size / float32(face.Upem())
	dx := req.Offset

	// Bounds of every point, control points included: a conservative box.
	minX, minY := float32(math.MaxFloat32), float32(math.MaxFloat32)
	maxX, maxY := -minX, -minY
	for _, seg := range outline.Segments {
		for _, p := range seg.ArgsSlice() {
			x, y := p.X*s+dx, -p.Y*s
			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
	}
	left := int(math.Floor(float64(minX)))
	top := int(math.Floor(float64(minY)))
	w := int(math.Ceil(float64(maxX))) - left
	h := int(math.Ceil(float64(maxY))) - top
	if w <= 0 || h <= 0 {
		return true
	}

	ox, oy := dx-float32(left), -float32(top)
	r.z.Reset(w, h)
	r.z.DrawOp = imgdraw.Src // Reset restores Over; Src overwrites reused Pix
	open := false
	for _, seg := range outline.Segments {
		a := seg.Args
		switch seg.Op {
		case ot.SegmentOpMoveTo:
			if open {
				r.z.ClosePath()
			}
			r.z.MoveTo(a[0].X*s+ox, oy-a[0].Y*s)
			open = true
		case ot.SegmentOpLineTo:
			r.z.LineTo(a[0].X*s+ox, oy-a[0].Y*s)
		case ot.SegmentOpQuadTo:
			r.z.QuadTo(a[0].X*s+ox, oy-a[0].Y*s, a[1].X*s+ox, oy-a[1].Y*s)
		case ot.SegmentOpCubeTo:
			r.z.CubeTo(a[0].X*s+ox, oy-a[0].Y*s, a[1].X*s+ox, oy-a[1].Y*s, a[2].X*s+ox, oy-a[2].Y*s)
		}
	}
	if open {
		r.z.ClosePath()
	}

	n := w * h
	if cap(dst.Pix) < n {
		dst.Pix = make([]byte, n)
	}
	dst.Pix = dst.Pix[:n]
	r.alpha = image.Alpha{Pix: dst.Pix, Stride: w, Rect: image.Rect(0, 0, w, h)}
	r.z.Draw(&r.alpha, r.alpha.Rect, image.Opaque, image.Point{})
	dst.W, dst.H, dst.Left, dst.Top = w, h, left, top
	return true
}

// TestColorFont is a synthetic color font, kept for the demo scene and tests
// of the RGBA color-glyph path (atlas, upload, shader slot t1). Real color
// glyphs come from sbix fallback faces (sbixRasterizer).
// Each glyph is a hue-swept disc one em wide; the GID picks the hue.
const TestColorFont draw.FontID = 250

// syntheticColor rasterizes TestColorFont glyphs as premultiplied RGBA.
type syntheticColor struct{}

func (syntheticColor) Rasterize(req GlyphRequest, dst *Bitmap) bool {
	size := int(math.Ceil(float64(req.Size)))
	if size <= 0 {
		*dst = Bitmap{Pix: dst.Pix[:0]}
		return true
	}
	n := size * size * 4
	pix := dst.Pix[:0]
	if cap(pix) < n {
		pix = make([]byte, n)
	}
	pix = pix[:n]
	c := float64(req.Size) / 2
	rad := c - 0.5
	shift := float64(req.GID%12) / 12
	for y := range size {
		for x := range size {
			fx := float64(x) + 0.5 - c - float64(req.Offset)
			fy := float64(y) + 0.5 - c
			a := math.Min(1, math.Max(0, rad-math.Hypot(fx, fy)+0.5))
			hue := math.Atan2(fy, fx)/(2*math.Pi) + 0.5 + shift
			r, g, b := hueRGB(hue)
			i := (y*size + x) * 4
			pix[i] = uint8(r*a*255 + 0.5)
			pix[i+1] = uint8(g*a*255 + 0.5)
			pix[i+2] = uint8(b*a*255 + 0.5)
			pix[i+3] = uint8(a*255 + 0.5)
		}
	}
	// One em square sitting on the baseline, like an emoji.
	*dst = Bitmap{W: size, H: size, Left: 0, Top: -int(math.Round(float64(req.Size) * 0.85)), Color: true, Pix: pix}
	return true
}

func hueRGB(h float64) (r, g, b float64) {
	h -= math.Floor(h)
	f := func(n float64) float64 {
		k := math.Mod(n+h*6, 6)
		return 1 - math.Max(0, math.Min(math.Min(k, 4-k), 1))
	}
	return f(5), f(3), f(1)
}

// sbixRasterizer draws color bitmap glyphs from a fallback face's sbix
// table (Apple Color Emoji), read from disk glyph by glyph.
type sbixRasterizer struct{}

func (sbixRasterizer) Rasterize(req GlyphRequest, dst *Bitmap) bool {
	*dst = Bitmap{Pix: dst.Pix[:0]}
	ff := fallbackByID(req.Font)
	if ff == nil || ff.sbix == nil {
		return false
	}
	g, ok := ff.sbix.glyph(req.GID, req.Size)
	if !ok {
		return true // no bitmap (a selector or joiner glyph): nothing to draw
	}
	// Bitmaps sit on whole pixels; req.Offset is ignored (the batch snaps
	// color glyphs, see gfx.Batch).
	return bitmapGlyph(g.png, g.ppem, g.originX, g.originY, req.Size, dst)
}

// fontRouter sends each font to its rasterizer: the synthetic color font and
// sbix fallback faces to the color rasterizers, everything else (embedded
// and fallback outlines) to the vector rasterizer.
type fontRouter struct {
	outline Rasterizer
	color   Rasterizer
	sbix    Rasterizer
}

func (f fontRouter) Rasterize(req GlyphRequest, dst *Bitmap) bool {
	switch {
	case req.Font == TestColorFont:
		return f.color.Rasterize(req, dst)
	case IsColorFont(req.Font):
		return f.sbix.Rasterize(req, dst)
	}
	return f.outline.Rasterize(req, dst)
}

// NewDefaultRasterizer returns the rasterizer the app uses: x/image/vector
// for the embedded fonts and outline fallbacks, sbix for color emoji, plus
// the synthetic color test font.
func NewDefaultRasterizer() (Rasterizer, error) {
	v, err := NewVectorRasterizer()
	if err != nil {
		return nil, err
	}
	return fontRouter{outline: v, color: syntheticColor{}, sbix: sbixRasterizer{}}, nil
}
