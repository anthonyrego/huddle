package gfx

import (
	"math"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/geom"
)

// Paths follow the rules the diagram SVG writer defines (see
// internal/diagram/svg): a path's end point is its arrowhead's tip, the
// stroke stops geom.EndTrim short of it, dashes follow geom.Dash with butt
// caps, solid strokes have round caps, joins are round, and arrowheads
// follow geom.Arrow.
//
// A stroke is one KindStroke instance per segment: the shader computes the
// exact distance to the segment (a capsule), so lines stay crisp at any
// zoom, and neighbors split their shared round join along the line through
// the joint (see strokeDistance in quad.frag.hlsl). A fill is triangulated
// on the CPU into KindTriangle instances that antialias only the polygon's
// own edges.

// pathScratch is reused between paths so tessellating allocates nothing in
// steady state.
type pathScratch struct {
	pts   []draw.Point // the path in target pixels
	piece []draw.Point // one dash
	kept  []draw.Point // a polyline without repeated points
	dirs  []draw.Point // unit direction of each segment
	poly  []draw.Point // a polygon being filled
	idx   []int32      // ear clipping: remaining polygon corners
}

// arrowPad is how far an arrowhead can reach past its path's points, in
// logical units, for culling (a triangle's half-base at width 3 is < 7).
const arrowPad = 10

// addPath tessellates one path. t maps logical units to target pixels.
func (b *Batch) addPath(p *draw.Path, t draw.Transform, clip pxRect) {
	if len(p.Points) < 2 {
		return
	}
	stroke := p.Stroke.A > 0 && p.Width > 0
	fill := p.Closed && p.Fill.A > 0
	if !stroke && !fill {
		return
	}
	if b.cur.xform == Identity {
		x0, y0, x1, y1 := bounds(p.Points)
		pad := p.Width/2 + arrowPad
		r := rectPx(t, draw.Rect{X: x0 - pad, Y: y0 - pad, W: x1 - x0 + 2*pad, H: y1 - y0 + 2*pad})
		if outside(clip, r, 1) {
			return
		}
	}
	s := &b.path
	s.pts = s.pts[:0]
	for _, q := range p.Points {
		s.pts = append(s.pts, t.Apply(q))
	}
	if fill {
		b.fillPolygon(s.pts, rgba(p.Fill))
	}
	if !stroke {
		return
	}
	w := p.Width * t.Scale
	col := rgba(p.Stroke)
	line := s.pts
	if p.Closed {
		if p.Dashed {
			line = append(line, line[0])
		}
	} else {
		line = trimEnd(line, geom.EndTrim(p.ArrowEnd, p.Width)*t.Scale)
		line = trimStart(line, geom.EndTrim(p.ArrowStart, p.Width)*t.Scale)
	}
	if p.Dashed {
		on, off := geom.Dash(p.Width)
		b.dashPolyline(line, on*t.Scale, off*t.Scale, w, col, clip)
	} else {
		b.strokePolyline(line, p.Closed, false, false, w, col, clip)
	}
	if !p.Closed {
		b.arrow(p.ArrowEnd, p, true, t, w, col, clip)
		b.arrow(p.ArrowStart, p, false, t, w, col, clip)
	}
}

func bounds(pts []draw.Point) (x0, y0, x1, y1 float32) {
	x0, y0 = pts[0].X, pts[0].Y
	x1, y1 = x0, y0
	for _, q := range pts[1:] {
		x0, y0 = min(x0, q.X), min(y0, q.Y)
		x1, y1 = max(x1, q.X), max(y1, q.Y)
	}
	return
}

func dist(a, b draw.Point) float32 {
	return float32(math.Hypot(float64(b.X-a.X), float64(b.Y-a.Y)))
}

func lerp(a, b draw.Point, t float32) draw.Point {
	return draw.Point{X: a.X + (b.X-a.X)*t, Y: a.Y + (b.Y-a.Y)*t}
}

// trimEnd shortens pts by d at its end, in place (geom.TrimEnd without the
// copy).
func trimEnd(pts []draw.Point, d float32) []draw.Point {
	for d > 0 && len(pts) >= 2 {
		n := len(pts)
		a, b := pts[n-2], pts[n-1]
		seg := dist(a, b)
		if seg > d {
			pts[n-1] = lerp(a, b, (seg-d)/seg)
			return pts
		}
		d -= seg
		pts = pts[:n-1]
	}
	return pts
}

// trimStart shortens pts by d at its start, in place.
func trimStart(pts []draw.Point, d float32) []draw.Point {
	for d > 0 && len(pts) >= 2 {
		a, b := pts[1], pts[0]
		seg := dist(a, b)
		if seg > d {
			pts[0] = lerp(a, b, (seg-d)/seg)
			return pts
		}
		d -= seg
		pts = pts[1:]
	}
	return pts
}

// minSeg is the shortest segment worth drawing, in pixels. Shorter ones
// are merged into their neighbors: they carry no visible length, and their
// direction is noise that would skew the joints around them.
const minSeg = 1e-3

// strokePolyline emits one stroke instance per segment of q. closed joins
// the last point back to the first; butt flags square off the open ends.
func (b *Batch) strokePolyline(q []draw.Point, closed, buttStart, buttEnd bool, w float32, col [4]uint8, clip pxRect) {
	s := &b.path
	// Drop repeated points (and the closing duplicate of a closed ring).
	kept := s.kept[:0]
	for _, p := range q {
		if len(kept) == 0 || dist(kept[len(kept)-1], p) > minSeg {
			kept = append(kept, p)
		}
	}
	if closed && len(kept) > 2 && dist(kept[0], kept[len(kept)-1]) <= minSeg {
		kept = kept[:len(kept)-1]
	}
	s.kept = kept
	n := len(kept)
	if n < 2 {
		if n == 1 && !buttStart && !buttEnd && !closed {
			// A zero-length stroke with round caps is a dot, as in SVG.
			b.segment(kept[0], kept[0], draw.Point{}, draw.Point{}, false, false, w, col, clip)
		}
		return
	}
	segs := n - 1
	if closed {
		segs = n
	}
	s.dirs = s.dirs[:0]
	for i := range segs {
		a, c := kept[i], kept[(i+1)%n]
		l := dist(a, c)
		s.dirs = append(s.dirs, draw.Point{X: (c.X - a.X) / l, Y: (c.Y - a.Y) / l})
	}
	for i := range segs {
		var ta, tb draw.Point
		if closed || i > 0 {
			ta = bisector(s.dirs[(i+segs-1)%segs], s.dirs[i])
		}
		if closed || i < segs-1 {
			tb = bisector(s.dirs[i], s.dirs[(i+1)%segs])
		}
		b.segment(kept[i], kept[(i+1)%n], ta, tb, !closed && i == 0 && buttStart, !closed && i == segs-1 && buttEnd, w, col, clip)
	}
}

// bisector is the stroke tangent at a joint between segments with unit
// directions u and v: the line through the joint normal to it splits the
// join between them. A full reversal has none; both segments then draw
// their whole round end.
func bisector(u, v draw.Point) draw.Point {
	x, y := u.X+v.X, u.Y+v.Y
	l := float32(math.Hypot(float64(x), float64(y)))
	if l < 1e-4 {
		return draw.Point{}
	}
	return draw.Point{X: x / l, Y: y / l}
}

// segment emits one stroke instance from a to c.
func (b *Batch) segment(a, c, ta, tc draw.Point, buttA, buttC bool, w float32, col [4]uint8, clip pxRect) {
	hw := w / 2
	x0, y0 := min(a.X, c.X)-hw, min(a.Y, c.Y)-hw
	r := [4]float32{x0, y0, max(a.X, c.X) + hw - x0, max(a.Y, c.Y) + hw - y0}
	if b.culls(clip, r, 1) {
		return
	}
	inst := Instance{
		Rect:   r,
		UV:     [4]float32{ta.X, ta.Y, tc.X, tc.Y},
		Color:  col,
		Radii:  [4]float32{a.X, a.Y, c.X, c.Y},
		Params: [4]float32{w, 1, float32(KindStroke), 0},
	}
	if buttA {
		inst.BorderColor[0] = 255
	}
	if buttC {
		inst.BorderColor[1] = 255
	}
	b.Add(inst)
}

// dashPolyline strokes q with an on/off dash pattern starting at its first
// point (SVG's stroke-dasharray with no offset). Each dash is its own
// polyline with butt caps; a dash that runs around a corner keeps a round
// join there.
func (b *Batch) dashPolyline(q []draw.Point, on, off, w float32, col [4]uint8, clip pxRect) {
	if len(q) < 2 || on <= 0 {
		return
	}
	s := &b.path
	piece := s.piece[:0]
	drawing, left := true, on
	piece = append(piece, q[0])
	flush := func() {
		if len(piece) >= 2 {
			b.strokePolyline(piece, false, true, true, w, col, clip)
		}
		piece = piece[:0]
	}
	for i := 1; i < len(q); i++ {
		a, c := q[i-1], q[i]
		seg := dist(a, c)
		pos := float32(0)
		for seg-pos > left {
			pos += left
			p := lerp(a, c, pos/seg)
			if drawing {
				piece = append(piece, p)
				flush()
				left = off
			} else {
				piece = append(piece[:0], p)
				left = on
			}
			drawing = !drawing
		}
		left -= seg - pos
		if drawing {
			piece = append(piece, c)
		}
	}
	if drawing {
		flush()
	}
	s.piece = piece[:0]
}

// arrow draws the arrowhead at one end of an open path, from the path's
// logical points (the tip is the untrimmed end point).
func (b *Batch) arrow(kind draw.Arrow, p *draw.Path, end bool, t draw.Transform, w float32, col [4]uint8, clip pxRect) {
	if kind == draw.ArrowNone {
		return
	}
	tip, prev := geom.Direction(p.Points, end, geom.ArrowLength(kind, p.Width)+2)
	h := geom.Arrow(kind, tip, prev, p.Width)
	switch kind {
	case draw.ArrowTriangle:
		b.triangle(t.Apply(h.Triangle[0]), t.Apply(h.Triangle[1]), t.Apply(h.Triangle[2]), [3]bool{true, true, true}, col, clip)
	case draw.ArrowCircle:
		c, r := t.Apply(h.Center), h.Radius*t.Scale
		rect := [4]float32{c.X - r, c.Y - r, 2 * r, 2 * r}
		if b.culls(clip, rect, 1) {
			return
		}
		b.Add(Instance{Rect: rect, Color: col, Radii: [4]float32{r, r, r, r}, Params: [4]float32{0, 1, float32(KindShape), 0}})
	case draw.ArrowCross:
		for _, s := range h.Cross {
			b.segment(t.Apply(s[0]), t.Apply(s[1]), draw.Point{}, draw.Point{}, false, false, w, col, clip)
		}
	}
}

// triangle emits one filled triangle. ext flags the edges v0v1, v1v2 and
// v2v0 that are the polygon's outline.
func (b *Batch) triangle(v0, v1, v2 draw.Point, ext [3]bool, col [4]uint8, clip pxRect) {
	area := (v1.X-v0.X)*(v2.Y-v0.Y) - (v1.Y-v0.Y)*(v2.X-v0.X)
	if area > -1e-6 && area < 1e-6 {
		return
	}
	x0, y0 := min(v0.X, v1.X, v2.X), min(v0.Y, v1.Y, v2.Y)
	r := [4]float32{x0, y0, max(v0.X, v1.X, v2.X) - x0, max(v0.Y, v1.Y, v2.Y) - y0}
	if b.culls(clip, r, 1) {
		return
	}
	inst := Instance{
		Rect:   r,
		UV:     [4]float32{v2.X, v2.Y, 0, 0},
		Color:  col,
		Radii:  [4]float32{v0.X, v0.Y, v1.X, v1.Y},
		Params: [4]float32{0, 1, float32(KindTriangle), 0},
	}
	for i, e := range ext {
		if e {
			inst.BorderColor[i] = 255
		}
	}
	b.Add(inst)
}

// fillPolygon triangulates a simple polygon (either winding): a fan when it
// is convex, ear clipping otherwise.
func (b *Batch) fillPolygon(q []draw.Point, col [4]uint8) {
	s := &b.path
	clip := b.cur.scissor
	// Drop repeats, the closing duplicate and collinear corners: they make
	// zero-area triangles and hide outline edges behind them.
	poly := s.poly[:0]
	for _, p := range q {
		if len(poly) == 0 || dist(poly[len(poly)-1], p) > minSeg {
			poly = append(poly, p)
		}
	}
	for len(poly) > 2 && dist(poly[0], poly[len(poly)-1]) <= minSeg {
		poly = poly[:len(poly)-1]
	}
	for changed := true; changed && len(poly) > 3; {
		changed = false
		for i := 0; i < len(poly) && len(poly) > 3; i++ {
			a, c, d := poly[(i+len(poly)-1)%len(poly)], poly[i], poly[(i+1)%len(poly)]
			if cr := cross(a, c, d); cr > -1e-4 && cr < 1e-4 && (c.X-a.X)*(d.X-c.X)+(c.Y-a.Y)*(d.Y-c.Y) >= 0 {
				poly = append(poly[:i], poly[i+1:]...)
				changed = true
				i--
			}
		}
	}
	s.poly = poly
	n := len(poly)
	if n < 3 {
		return
	}
	if convex(poly) {
		for i := 1; i+1 < n; i++ {
			b.triangle(poly[0], poly[i], poly[i+1], [3]bool{i == 1, true, i+2 == n}, col, clip)
		}
		return
	}
	// Ear clipping over counterclockwise corners (y down: positive cross
	// products turn right on screen, which is what "counterclockwise" is in
	// y-up terms; only consistency matters).
	idx := s.idx[:0]
	for i := range n {
		idx = append(idx, int32(i))
	}
	if signedArea(poly) < 0 {
		for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
			idx[i], idx[j] = idx[j], idx[i]
		}
	}
	adjacent := func(i, j int32) bool { return (i+1)%int32(n) == j || (j+1)%int32(n) == i }
	for len(idx) > 3 {
		m := len(idx)
		found := false
		for k := 0; k < m; k++ {
			ia, ib, ic := idx[(k+m-1)%m], idx[k], idx[(k+1)%m]
			a, c, d := poly[ia], poly[ib], poly[ic]
			if cross(a, c, d) <= 0 {
				continue // reflex
			}
			ear := true
			for _, j := range idx {
				if j != ia && j != ib && j != ic && inTriangle(poly[j], a, c, d) {
					ear = false
					break
				}
			}
			if !ear {
				continue
			}
			b.triangle(a, c, d, [3]bool{adjacent(ia, ib), adjacent(ib, ic), adjacent(ic, ia)}, col, clip)
			idx = append(idx[:k], idx[k+1:]...)
			found = true
			break
		}
		if !found {
			// Self-intersecting or degenerate: fan what's left rather than
			// dropping it.
			for k := 1; k+1 < len(idx); k++ {
				ia, ib, ic := idx[0], idx[k], idx[k+1]
				b.triangle(poly[ia], poly[ib], poly[ic], [3]bool{adjacent(ia, ib), adjacent(ib, ic), adjacent(ic, ia)}, col, clip)
			}
			idx = idx[:0]
			break
		}
	}
	if len(idx) == 3 {
		ia, ib, ic := idx[0], idx[1], idx[2]
		b.triangle(poly[ia], poly[ib], poly[ic], [3]bool{adjacent(ia, ib), adjacent(ib, ic), adjacent(ic, ia)}, col, clip)
	}
	s.idx = idx[:0]
}

// cross is the z of (c-a)×(d-c): the turn at c.
func cross(a, c, d draw.Point) float32 {
	return (c.X-a.X)*(d.Y-c.Y) - (c.Y-a.Y)*(d.X-c.X)
}

func convex(poly []draw.Point) bool {
	n := len(poly)
	pos, neg := false, false
	for i := range n {
		cr := cross(poly[(i+n-1)%n], poly[i], poly[(i+1)%n])
		pos = pos || cr > 1e-4
		neg = neg || cr < -1e-4
	}
	return !(pos && neg)
}

func signedArea(poly []draw.Point) float32 {
	var a float32
	for i := range poly {
		p, q := poly[i], poly[(i+1)%len(poly)]
		a += p.X*q.Y - q.X*p.Y
	}
	return a / 2
}

// inTriangle reports whether p is inside or on triangle abc (positive
// winding, as the ear clipper keeps it).
func inTriangle(p, a, b, c draw.Point) bool {
	return cross(a, b, p) >= 0 && cross(b, c, p) >= 0 && cross(c, a, p) >= 0
}
