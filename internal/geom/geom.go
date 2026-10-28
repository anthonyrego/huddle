// Package geom holds the path geometry that drawing and the GPU renderer
// must agree on: arcs, curve flattening, clipping, dashes and arrowheads.
//
// Copied from an earlier project of the author's
// (without the mermaid node shapes).
package geom

import (
	"math"

	"github.com/anthonyrego/huddle/internal/draw"
)

type P = draw.Point

func pt(x, y float32) P { return P{X: x, Y: y} }

// ellipse returns n+1 points on an elliptical arc from angle a0 to a1
// (radians, y down, so increasing angles run clockwise on screen).
func ellipse(cx, cy, rx, ry float32, a0, a1 float64, n int) []P {
	pts := make([]P, 0, n+1)
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		pts = append(pts, P{X: cx + rx*float32(math.Cos(a)), Y: cy + ry*float32(math.Sin(a))})
	}
	return pts
}

// Arc is ellipse, exported for drawing the visible cylinder cap.
func Arc(cx, cy, rx, ry float32, a0, a1 float64, n int) []P {
	return ellipse(cx, cy, rx, ry, a0, a1, n)
}

// ClipSegment returns the first point where the segment from a to b crosses
// the closed polygon, walking from a. ok is false if it never does.
func ClipSegment(poly []P, a, b P) (P, bool) {
	best := float32(2)
	var hit P
	for i := range poly {
		c, d := poly[i], poly[(i+1)%len(poly)]
		if t, ok := segIntersect(a, b, c, d); ok && t < best {
			best = t
			hit = P{X: a.X + (b.X-a.X)*t, Y: a.Y + (b.Y-a.Y)*t}
		}
	}
	return hit, best <= 1
}

// segIntersect returns t along ab where it meets cd.
func segIntersect(a, b, c, d P) (float32, bool) {
	rx, ry := b.X-a.X, b.Y-a.Y
	sx, sy := d.X-c.X, d.Y-c.Y
	den := rx*sy - ry*sx
	if den == 0 {
		return 0, false
	}
	qx, qy := c.X-a.X, c.Y-a.Y
	t := (qx*sy - qy*sx) / den
	u := (qx*ry - qy*rx) / den
	if t < 0 || t > 1 || u < 0 || u > 1 {
		return 0, false
	}
	return t, true
}

// RectPoly is a rect as a polygon.
func RectPoly(r draw.Rect) []P {
	return []P{pt(r.X, r.Y), pt(r.Right(), r.Y), pt(r.Right(), r.Bottom()), pt(r.X, r.Bottom())}
}

// Basis flattens d3's curveBasis (the uniform cubic B-spline mermaid draws
// flowchart edges with) through pts into a polyline. The curve starts at the
// first point and ends at the last, tangent to the first and last segments.
func Basis(pts []P, steps int) []P {
	n := len(pts)
	if n < 3 {
		return append([]P(nil), pts...)
	}
	out := []P{pts[0]}
	// d3: moveTo(p0); lineTo((5p0+p1)/6); then for each new point a bezier
	// from the previous B-spline knots; finish with the last two points.
	x0, y0 := pts[0].X, pts[0].Y
	x1, y1 := pts[0].X, pts[0].Y
	cur := P{X: (5*x0 + pts[1].X) / 6, Y: (5*y0 + pts[1].Y) / 6}
	out = append(out, cur)
	bez := func(x, y float32) {
		c1 := P{X: (2*x0 + x1) / 3, Y: (2*y0 + y1) / 3}
		c2 := P{X: (x0 + 2*x1) / 3, Y: (y0 + 2*y1) / 3}
		end := P{X: (x0 + 4*x1 + x) / 6, Y: (y0 + 4*y1 + y) / 6}
		out = appendCubic(out, cur, c1, c2, end, steps)
		cur = end
	}
	x0, y0, x1, y1 = x1, y1, pts[1].X, pts[1].Y
	for i := 2; i < n; i++ {
		bez(pts[i].X, pts[i].Y)
		x0, y0, x1, y1 = x1, y1, pts[i].X, pts[i].Y
	}
	// lineEnd for point>=3: point(x1,y1) then lineTo(x1,y1).
	bez(x1, y1)
	out = append(out, pts[n-1])
	return dedupe(out)
}

func appendCubic(out []P, p0, c1, c2, p1 P, steps int) []P {
	for i := 1; i <= steps; i++ {
		t := float32(i) / float32(steps)
		u := 1 - t
		a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		out = append(out, P{
			X: a*p0.X + b*c1.X + c*c2.X + d*p1.X,
			Y: a*p0.Y + b*c1.Y + c*c2.Y + d*p1.Y,
		})
	}
	return out
}

func dedupe(pts []P) []P {
	out := pts[:1]
	for _, p := range pts[1:] {
		q := out[len(out)-1]
		if abs(p.X-q.X) > 0.01 || abs(p.Y-q.Y) > 0.01 {
			out = append(out, p)
		}
	}
	return out
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// ArrowLength is how far an arrowhead of the given kind extends back from
// the path's end point (the tip) for a stroke of the given width. Renderers
// stop the stroke this far from the tip.
func ArrowLength(kind draw.Arrow, width float32) float32 {
	switch kind {
	case draw.ArrowTriangle:
		return 3*width + 6
	case draw.ArrowCircle:
		return 2 * ArrowCircleRadius(width)
	case draw.ArrowCross:
		return 0
	}
	return 0
}

// ArrowCircleRadius is the radius of an ArrowCircle end marker.
func ArrowCircleRadius(width float32) float32 { return width + 2.5 }

// ArrowHead returns the arrowhead polygon (triangle), or the circle center
// and radius (circle), or the two strokes of an x (cross) for a path ending
// at tip and arriving from direction of prev.
type ArrowHead struct {
	Kind     draw.Arrow
	Triangle [3]P
	Center   P
	Radius   float32
	Cross    [2][2]P
}

// Arrow computes the arrowhead for a path end.
func Arrow(kind draw.Arrow, tip, prev P, width float32) ArrowHead {
	dx, dy := tip.X-prev.X, tip.Y-prev.Y
	l := float32(math.Hypot(float64(dx), float64(dy)))
	if l == 0 {
		dx, dy, l = 1, 0, 1
	}
	ux, uy := dx/l, dy/l // along the path, toward the tip
	nx, ny := -uy, ux    // normal
	h := ArrowHead{Kind: kind}
	switch kind {
	case draw.ArrowTriangle:
		L := ArrowLength(kind, width)
		half := L * 0.42
		base := P{X: tip.X - ux*L, Y: tip.Y - uy*L}
		h.Triangle = [3]P{tip, {X: base.X + nx*half, Y: base.Y + ny*half}, {X: base.X - nx*half, Y: base.Y - ny*half}}
	case draw.ArrowCircle:
		r := ArrowCircleRadius(width)
		h.Center, h.Radius = P{X: tip.X - ux*r, Y: tip.Y - uy*r}, r
	case draw.ArrowCross:
		s := width + 3.5
		c := P{X: tip.X - ux*(s+1), Y: tip.Y - uy*(s+1)}
		ax, ay := (ux+nx)*s*0.7071, (uy+ny)*s*0.7071
		bx, by := (ux-nx)*s*0.7071, (uy-ny)*s*0.7071
		h.Cross = [2][2]P{{{X: c.X - ax, Y: c.Y - ay}, {X: c.X + ax, Y: c.Y + ay}}, {{X: c.X - bx, Y: c.Y - by}, {X: c.X + bx, Y: c.Y + by}}}
	}
	return h
}

// TrimEnd shortens a polyline by d at its end (for stopping a stroke short of
// an arrowhead), returning a new slice.
func TrimEnd(pts []P, d float32) []P {
	out := append([]P(nil), pts...)
	for d > 0 && len(out) >= 2 {
		a, b := out[len(out)-2], out[len(out)-1]
		seg := float32(math.Hypot(float64(b.X-a.X), float64(b.Y-a.Y)))
		if seg > d {
			t := (seg - d) / seg
			out[len(out)-1] = P{X: a.X + (b.X-a.X)*t, Y: a.Y + (b.Y-a.Y)*t}
			return out
		}
		d -= seg
		out = out[:len(out)-1]
	}
	return out
}

// TrimStart shortens a polyline by d at its start.
func TrimStart(pts []P, d float32) []P {
	rev := make([]P, len(pts))
	for i, p := range pts {
		rev[len(pts)-1-i] = p
	}
	rev = TrimEnd(rev, d)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Direction returns the point a little way back along the path from its end,
// for orienting an arrowhead on a flattened curve.
func Direction(pts []P, fromEnd bool, dist float32) (tip, prev P) {
	n := len(pts)
	if fromEnd {
		tip = pts[n-1]
		for i := n - 2; i >= 0; i-- {
			prev = pts[i]
			if hypot(tip, prev) >= dist {
				break
			}
		}
		return tip, prev
	}
	tip = pts[0]
	for i := 1; i < n; i++ {
		prev = pts[i]
		if hypot(tip, prev) >= dist {
			break
		}
	}
	return tip, prev
}

func hypot(a, b P) float32 { return float32(math.Hypot(float64(a.X-b.X), float64(a.Y-b.Y))) }

// Dash returns the on and off lengths of a dashed stroke of the given width.
func Dash(width float32) (on, off float32) { return max(3, 2.5*width), max(3, 2*width) }

// EndTrim is how far a stroke stops short of its end point so it doesn't
// poke through the arrowhead drawn there.
func EndTrim(kind draw.Arrow, width float32) float32 {
	switch kind {
	case draw.ArrowTriangle:
		return ArrowLength(kind, width) * 0.8
	case draw.ArrowCircle:
		return 2 * ArrowCircleRadius(width)
	}
	return 0
}
