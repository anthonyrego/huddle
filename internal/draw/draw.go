// Package draw is the display list every renderer consumes. Layout and
// widgets emit a List and internal/gfx draws it on the GPU, on screen or
// offscreen.
//
// Coordinates are logical units (points), y down. Renderers multiply by the
// display scale. The package is pure Go and must never import SDL.
//
// Copied from an earlier project of the author's.
package draw

// Point is a position in logical units.
type Point struct{ X, Y float32 }

// Rect is an axis-aligned rectangle in logical units.
type Rect struct{ X, Y, W, H float32 }

// Right returns the x coordinate of the right edge.
func (r Rect) Right() float32 { return r.X + r.W }

// Bottom returns the y coordinate of the bottom edge.
func (r Rect) Bottom() float32 { return r.Y + r.H }

// Contains reports whether p lies inside r.
func (r Rect) Contains(p Point) bool {
	return p.X >= r.X && p.X < r.Right() && p.Y >= r.Y && p.Y < r.Bottom()
}

// Intersect returns the overlap of r and o, or an empty rect.
func (r Rect) Intersect(o Rect) Rect {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.Right(), o.Right()), min(r.Bottom(), o.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{X: x0, Y: y0}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// Empty reports whether r has no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Inset shrinks r by d on every side.
func (r Rect) Inset(d float32) Rect {
	return Rect{X: r.X + d, Y: r.Y + d, W: r.W - 2*d, H: r.H - 2*d}
}

// Color is a straight (non-premultiplied) sRGB color.
type Color struct{ R, G, B, A uint8 }

// RGB returns an opaque color from a 0xRRGGBB value.
func RGB(hex uint32) Color {
	return Color{R: uint8(hex >> 16), G: uint8(hex >> 8), B: uint8(hex), A: 0xff}
}

// WithAlpha returns c with its alpha replaced.
func (c Color) WithAlpha(a uint8) Color { c.A = a; return c }

// Radii are corner radii: top-left, top-right, bottom-right, bottom-left.
type Radii [4]float32

// Uniform returns equal radii on all corners.
func Uniform(r float32) Radii { return Radii{r, r, r, r} }

// FontID identifies one of the embedded faces (see internal/text).
type FontID uint8

// GlyphPos is one shaped glyph, positioned relative to its run's origin.
type GlyphPos struct {
	GID  uint32
	X, Y float32
}

// GlyphRun is a sequence of shaped glyphs in one face, size and color.
// Origin is the baseline start. Text is the source text for the run, kept
// for the SVG writer, hit-testing and "copy source".
type GlyphRun struct {
	Origin Point
	Font   FontID
	Size   float32
	Color  Color
	Glyphs []GlyphPos
	Text   string
}

// Arrow selects an arrowhead style at a path end.
type Arrow uint8

const (
	ArrowNone Arrow = iota
	ArrowTriangle
	ArrowCircle
	ArrowCross
)

// Path is a polyline, optionally closed and filled. Diagram edges use
// Stroke with arrowheads; node outlines that are not rects use Fill.
type Path struct {
	Points     []Point
	Closed     bool
	Fill       Color
	Stroke     Color
	Width      float32
	Dashed     bool
	ArrowStart Arrow
	ArrowEnd   Arrow
}

// ImageID names an image the renderer has been given elsewhere.
type ImageID uint64

// Transform maps child coordinates to parent: p' = p*Scale + Offset.
type Transform struct {
	Scale  float32
	Offset Point
}

// Apply maps p through t.
func (t Transform) Apply(p Point) Point {
	return Point{X: p.X*t.Scale + t.Offset.X, Y: p.Y*t.Scale + t.Offset.Y}
}

// OpKind identifies an Op.
type OpKind uint8

const (
	OpRect OpKind = iota
	OpShadow
	OpGlyphs
	OpPath
	OpImage
	OpPushClip
	OpPopClip
	OpPushTransform
	OpPopTransform
)

// Op is one display-list entry. Which fields matter depends on Kind:
//
//	OpRect          Rect, Fill, Stroke, StrokeWidth, Radii
//	OpShadow        Rect, Fill (shadow color), Radii, Blur
//	OpGlyphs        index into the list's glyph runs (List.RunAt)
//	OpPath          index into the list's paths (List.PathAt)
//	OpImage         Rect, Image, Radii, Fill.A as opacity
//	OpPushClip      Rect
//	OpPushTransform Transform
type Op struct {
	Kind        OpKind
	Rect        Rect
	Fill        Color
	Stroke      Color
	StrokeWidth float32
	Radii       Radii
	Blur        float32
	Image       ImageID
	Transform   Transform
	index       int32
}

// List is an ordered display list. The zero value is ready to use; Reset
// reuses its memory between frames.
type List struct {
	ops    []Op
	glyphs []GlyphRun
	paths  []Path
}

// Reset clears the list, keeping allocated memory.
func (l *List) Reset() {
	l.ops = l.ops[:0]
	l.glyphs = l.glyphs[:0]
	l.paths = l.paths[:0]
}

// Ops returns the ops in draw order. The slice is only valid until the next
// mutation of the list.
func (l *List) Ops() []Op { return l.ops }

// RunAt returns the glyph run referenced by an OpGlyphs op.
func (l *List) RunAt(op Op) *GlyphRun { return &l.glyphs[op.index] }

// PathAt returns the path referenced by an OpPath op.
func (l *List) PathAt(op Op) *Path { return &l.paths[op.index] }

// Rect adds a filled rounded rect with an optional border.
func (l *List) Rect(r Rect, fill Color, radii Radii) {
	l.ops = append(l.ops, Op{Kind: OpRect, Rect: r, Fill: fill, Radii: radii})
}

// BorderedRect adds a rounded rect with a border drawn inside its bounds.
func (l *List) BorderedRect(r Rect, fill, stroke Color, width float32, radii Radii) {
	l.ops = append(l.ops, Op{Kind: OpRect, Rect: r, Fill: fill, Stroke: stroke, StrokeWidth: width, Radii: radii})
}

// Shadow adds a soft shadow for a rounded rect. Blur is the falloff width.
func (l *List) Shadow(r Rect, color Color, radii Radii, blur float32) {
	l.ops = append(l.ops, Op{Kind: OpShadow, Rect: r, Fill: color, Radii: radii, Blur: blur})
}

// Glyphs adds a shaped glyph run. The run is copied; its Glyphs slice is not.
func (l *List) Glyphs(run GlyphRun) {
	l.glyphs = append(l.glyphs, run)
	l.ops = append(l.ops, Op{Kind: OpGlyphs, index: int32(len(l.glyphs) - 1)})
}

// Path adds a polyline or polygon. The path is copied; its Points slice is not.
func (l *List) Path(p Path) {
	l.paths = append(l.paths, p)
	l.ops = append(l.ops, Op{Kind: OpPath, index: int32(len(l.paths) - 1)})
}

// Image adds an image scaled to r, clipped to the given radii.
func (l *List) Image(r Rect, id ImageID, radii Radii, opacity uint8) {
	l.ops = append(l.ops, Op{Kind: OpImage, Rect: r, Image: id, Radii: radii, Fill: Color{A: opacity}})
}

// PushClip restricts drawing to r intersected with the current clip.
func (l *List) PushClip(r Rect) {
	l.ops = append(l.ops, Op{Kind: OpPushClip, Rect: r})
}

// PopClip restores the clip in effect before the matching PushClip.
func (l *List) PopClip() {
	l.ops = append(l.ops, Op{Kind: OpPopClip})
}

// PushTransform composes t onto the current transform for following ops.
func (l *List) PushTransform(t Transform) {
	l.ops = append(l.ops, Op{Kind: OpPushTransform, Transform: t})
}

// PopTransform restores the transform in effect before the matching push.
func (l *List) PopTransform() {
	l.ops = append(l.ops, Op{Kind: OpPopTransform})
}
