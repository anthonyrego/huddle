package gfx

import (
	"math"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
)

// Batch is one frame's instances, split into segments. A segment is one draw
// call: it starts whenever the scissor, the GPU transform or a bound texture
// (glyph page, color page, image) changes. Instances that sample nothing
// (shapes, shadows) never break a segment over textures, and a segment that
// hasn't bound a slot yet takes the next texture for that slot for free.
type Batch struct {
	Instances []Instance
	// Glyphs resolves OpGlyphs to atlas entries. Nil skips text.
	Glyphs *text.GlyphCache

	segs []segment
	cur  segState
	path pathScratch
	// GlyphCount is the number of glyph instances added since Reset.
	GlyphCount int
}

// GPUTransform maps instance pixels to target pixels on the GPU:
// p' = p*Scale + (X, Y). Display-list transforms are applied on the CPU
// instead, so glyphs land on whole pixels; this one is for moving already
// built instances cheaply (a pan or zoom in progress, say).
type GPUTransform struct{ Scale, X, Y float32 }

// Identity is the GPU transform that leaves instances where they are.
var Identity = GPUTransform{Scale: 1}

// segState is what a segment binds. Texture slots use -1 (or imageSet
// false) for "not bound yet".
type segState struct {
	scissor    pxRect
	xform      GPUTransform
	glyphPage  int16
	colorPage  int16
	image      draw.ImageID
	imageBound bool
}

type segment struct {
	start, count uint32
	segState
}

// pxRect is a scissor rectangle in target pixels.
type pxRect struct{ x, y, w, h int32 }

// Reset starts a new frame for a target of the given pixel size. If a glyph
// cache is set, it also starts the cache's frame.
func (b *Batch) Reset(pw, ph int) {
	b.Instances = b.Instances[:0]
	b.segs = b.segs[:0]
	b.cur = segState{scissor: pxRect{0, 0, int32(pw), int32(ph)}, xform: Identity, glyphPage: -1, colorPage: -1}
	b.GlyphCount = 0
	if b.Glyphs != nil {
		b.Glyphs.BeginFrame()
	}
}

// Segments is the number of draw calls the batch needs.
func (b *Batch) Segments() int { return len(b.segs) }

// SetTransform sets the GPU transform for following instances.
func (b *Batch) SetTransform(t GPUTransform) {
	if t.Scale == 0 {
		t.Scale = 1
	}
	b.cur.xform = t
}

func (b *Batch) setScissor(r pxRect) { b.cur.scissor = r }

// open returns the current segment if it matches the scissor and transform,
// else starts one.
func (b *Batch) open() *segment {
	n := len(b.segs)
	if n > 0 {
		s := &b.segs[n-1]
		if s.scissor == b.cur.scissor && s.xform == b.cur.xform {
			return s
		}
	}
	b.segs = append(b.segs, segment{start: uint32(len(b.Instances)), segState: segState{
		scissor: b.cur.scissor, xform: b.cur.xform, glyphPage: -1, colorPage: -1,
	}})
	return &b.segs[n]
}

// split starts a new segment with the same scissor and transform.
func (b *Batch) split() *segment {
	b.segs = append(b.segs, segment{start: uint32(len(b.Instances)), segState: segState{
		scissor: b.cur.scissor, xform: b.cur.xform, glyphPage: -1, colorPage: -1,
	}})
	return &b.segs[len(b.segs)-1]
}

func (b *Batch) push(s *segment, inst Instance) {
	b.Instances = append(b.Instances, inst)
	s.count++
}

// Add appends an instance that samples no texture (shape or shadow).
func (b *Batch) Add(inst Instance) { b.push(b.open(), inst) }

// AddImage appends an instance drawn from image id.
func (b *Batch) AddImage(inst Instance, id draw.ImageID) {
	s := b.open()
	if s.imageBound && s.image != id {
		s = b.split()
	}
	s.image, s.imageBound = id, true
	b.push(s, inst)
}

// addGlyph appends a glyph instance sampling a coverage or color page.
func (b *Batch) addGlyph(inst Instance, page int16, color bool) {
	s := b.open()
	slot := &s.glyphPage
	if color {
		slot = &s.colorPage
	}
	if *slot >= 0 && *slot != page {
		s = b.split()
		slot = &s.glyphPage
		if color {
			slot = &s.colorPage
		}
	}
	*slot = page
	b.push(s, inst)
	b.GlyphCount++
}

func rgba(c draw.Color) [4]uint8 { return [4]uint8{c.R, c.G, c.B, c.A} }

// AddList translates a display list into instances. scale converts logical
// units to pixels.
func (b *Batch) AddList(l *draw.List, scale float32) { b.addOps(l, l.Ops(), scale) }

func (b *Batch) addOps(l *draw.List, ops []draw.Op, scale float32) {
	type xf = draw.Transform
	base := b.cur.scissor
	clips := []pxRect{base}
	xforms := []xf{{Scale: scale}}
	for _, op := range ops {
		t := xforms[len(xforms)-1]
		clip := clips[len(clips)-1]
		switch op.Kind {
		case draw.OpRect:
			r := rectPx(t, op.Rect)
			if b.culls(clip, r, 1) {
				continue
			}
			b.Add(Instance{
				Rect:        r,
				Color:       rgba(op.Fill),
				BorderColor: rgba(op.Stroke),
				Radii:       radiiPx(t, op.Radii),
				Params:      [4]float32{op.StrokeWidth * t.Scale, 1, float32(KindShape), 0},
			})
		case draw.OpShadow:
			blur := op.Blur * t.Scale
			r := rectPx(t, op.Rect)
			if b.culls(clip, r, blur+1) {
				continue
			}
			b.Add(Instance{
				Rect:   r,
				Color:  rgba(op.Fill),
				Radii:  radiiPx(t, op.Radii),
				Params: [4]float32{0, blur + 1, float32(KindShadow), blur},
			})
		case draw.OpImage:
			b.AddImage(Instance{
				Rect:   rectPx(t, op.Rect),
				UV:     [4]float32{0, 0, 1, 1},
				Color:  [4]uint8{255, 255, 255, op.Fill.A},
				Radii:  radiiPx(t, op.Radii),
				Params: [4]float32{0, 0, float32(KindImage), 0},
			}, op.Image)
		case draw.OpGlyphs:
			if b.Glyphs != nil {
				b.addRun(l.RunAt(op), t, clip)
			}
		case draw.OpPath:
			b.addPath(l.PathAt(op), t, clip)
		case draw.OpPushClip:
			r := rectPx(t, op.Rect)
			c := intersectPx(clip, pxRect{
				x: int32(math.Floor(float64(r[0]))), y: int32(math.Floor(float64(r[1]))),
				w: int32(math.Ceil(float64(r[0]+r[2]))) - int32(math.Floor(float64(r[0]))),
				h: int32(math.Ceil(float64(r[1]+r[3]))) - int32(math.Floor(float64(r[1]))),
			})
			clips = append(clips, c)
			b.setScissor(c)
		case draw.OpPopClip:
			if len(clips) > 1 {
				clips = clips[:len(clips)-1]
			}
			b.setScissor(clips[len(clips)-1])
		case draw.OpPushTransform:
			xforms = append(xforms, xf{
				Scale:  t.Scale * op.Transform.Scale,
				Offset: t.Apply(op.Transform.Offset),
			})
		case draw.OpPopTransform:
			if len(xforms) > 1 {
				xforms = xforms[:len(xforms)-1]
			}
		}
	}
	b.setScissor(base)
}

// addRun emits one instance per visible glyph. The baseline snaps to a whole
// pixel; each pen x snaps to the cache's subpixel bins. Glyphs outside the
// clip are skipped before they are looked up, so a long line scrolled
// sideways never rasterizes what can't be seen.
func (b *Batch) addRun(run *draw.GlyphRun, t draw.Transform, clip pxRect) {
	size := run.Size * t.Scale
	if size <= 0 || len(run.Glyphs) == 0 {
		return
	}
	o := t.Apply(run.Origin)
	baseline := float32(math.Round(float64(o.Y)))
	left, right := float32(math.Inf(-1)), float32(math.Inf(1))
	if b.cur.xform == Identity {
		// Generous vertical extent: ascenders and descenders stay within
		// 1.5 em.
		if baseline-1.5*size > float32(clip.y+clip.h) || baseline+1.5*size < float32(clip.y) {
			return
		}
		left, right = float32(clip.x)-2*size, float32(clip.x+clip.w)
	}
	if text.IsColorFont(run.Font) {
		b.addColorRun(run, t, o, baseline, size, left, right)
		return
	}
	c := b.Glyphs
	for _, g := range run.Glyphs {
		pen := o.X + g.X*t.Scale
		if pen > right {
			break // left to right: the rest are further right
		}
		if pen < left {
			continue
		}
		px, bin := c.Snap(pen)
		gl := c.Get(run.Font, g.GID, size, bin)
		if !gl.Drawable() {
			continue
		}
		y := baseline + float32(math.Round(float64(g.Y*t.Scale)))
		inst := Instance{
			Rect: [4]float32{float32(px + int(gl.Left)), y + float32(gl.Top), float32(gl.W), float32(gl.H)},
		}
		ps := float32(c.Gray.PageSize)
		if gl.Color {
			ps = float32(c.Color.PageSize)
		}
		inst.UV = [4]float32{float32(gl.X) / ps, float32(gl.Y) / ps, float32(gl.X+gl.W) / ps, float32(gl.Y+gl.H) / ps}
		if gl.Color {
			inst.Color = [4]uint8{255, 255, 255, run.Color.A}
			inst.Params[2] = float32(KindColorGlyph)
		} else {
			inst.Color = rgba(run.Color)
			inst.Params[2] = float32(KindGlyph)
		}
		b.addGlyph(inst, gl.Page, gl.Color)
	}
}

// addColorRun is addRun for color bitmap fonts (emoji). They are scaled
// images, not outlines, so each sits on a whole pixel and is rasterized
// once rather than once per subpixel bin. A separate loop keeps addRun's
// per-glyph path exactly as it was for text.
func (b *Batch) addColorRun(run *draw.GlyphRun, t draw.Transform, o draw.Point, baseline, size, left, right float32) {
	c := b.Glyphs
	ps := float32(c.Color.PageSize)
	for _, g := range run.Glyphs {
		pen := o.X + g.X*t.Scale
		if pen > right {
			break
		}
		if pen < left {
			continue
		}
		gl := c.Get(run.Font, g.GID, size, 0)
		if !gl.Drawable() || !gl.Color {
			continue
		}
		px := float32(math.Round(float64(pen)))
		y := baseline + float32(math.Round(float64(g.Y*t.Scale)))
		b.addGlyph(Instance{
			Rect:   [4]float32{px + float32(gl.Left), y + float32(gl.Top), float32(gl.W), float32(gl.H)},
			UV:     [4]float32{float32(gl.X) / ps, float32(gl.Y) / ps, float32(gl.X+gl.W) / ps, float32(gl.Y+gl.H) / ps},
			Color:  [4]uint8{255, 255, 255, run.Color.A},
			Params: [4]float32{2: float32(KindColorGlyph)},
		}, gl.Page, true)
	}
}

// culls reports whether an instance rect can be dropped: it misses the
// clip. Instance space is target space only under the identity GPU
// transform, so nothing is culled under any other.
func (b *Batch) culls(c pxRect, r [4]float32, pad float32) bool {
	return b.cur.xform == Identity && outside(c, r, pad)
}

// outside reports whether rect r (pixels, grown by pad) misses the clip.
func outside(c pxRect, r [4]float32, pad float32) bool {
	return r[0]-pad >= float32(c.x+c.w) || r[1]-pad >= float32(c.y+c.h) ||
		r[0]+r[2]+pad <= float32(c.x) || r[1]+r[3]+pad <= float32(c.y)
}

func rectPx(t draw.Transform, r draw.Rect) [4]float32 {
	p := t.Apply(draw.Point{X: r.X, Y: r.Y})
	return [4]float32{p.X, p.Y, r.W * t.Scale, r.H * t.Scale}
}

func radiiPx(t draw.Transform, r draw.Radii) [4]float32 {
	return [4]float32{r[0] * t.Scale, r[1] * t.Scale, r[2] * t.Scale, r[3] * t.Scale}
}

func intersectPx(a, b pxRect) pxRect {
	x0, y0 := max(a.x, b.x), max(a.y, b.y)
	x1, y1 := min(a.x+a.w, b.x+b.w), min(a.y+a.h, b.y+b.h)
	if x1 < x0 {
		x1 = x0
	}
	if y1 < y0 {
		y1 = y0
	}
	return pxRect{x0, y0, x1 - x0, y1 - y0}
}
