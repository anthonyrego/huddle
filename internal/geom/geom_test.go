package geom

import (
	"math"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
)

// inside reports whether p is inside the closed polygon (ray casting).
func inside(poly []P, p P) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.Y > p.Y) != (b.Y > p.Y) && p.X < (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y)+a.X {
			in = !in
		}
	}
	return in
}

func TestBasisKeepsEndpoints(t *testing.T) {
	pts := []P{{X: 0, Y: 0}, {X: 50, Y: 0}, {X: 50, Y: 50}, {X: 100, Y: 50}}
	out := Basis(pts, 8)
	if out[0] != pts[0] || out[len(out)-1] != pts[len(pts)-1] {
		t.Errorf("endpoints moved: %v ... %v", out[0], out[len(out)-1])
	}
	if len(out) < 10 {
		t.Errorf("curve not flattened: %d points", len(out))
	}
	if two := Basis(pts[:2], 8); len(two) != 2 {
		t.Errorf("a straight segment should stay 2 points, got %d", len(two))
	}
}

func TestClipSegment(t *testing.T) {
	sq := RectPoly(draw.Rect{X: 0, Y: 0, W: 10, H: 10})
	p, ok := ClipSegment(sq, P{X: 20, Y: 5}, P{X: 5, Y: 5})
	if !ok || p != (P{X: 10, Y: 5}) {
		t.Errorf("got %v %v", p, ok)
	}
	if _, ok := ClipSegment(sq, P{X: 20, Y: 20}, P{X: 30, Y: 30}); ok {
		t.Error("segment outside should not clip")
	}
}

func TestTrim(t *testing.T) {
	pts := []P{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}}
	end := TrimEnd(pts, 4)
	if last := end[len(end)-1]; math.Abs(float64(last.Y-6)) > 1e-4 || last.X != 10 {
		t.Errorf("TrimEnd: %v", end)
	}
	start := TrimStart(pts, 12)
	if first := start[0]; math.Abs(float64(first.Y-2)) > 1e-4 || first.X != 10 || len(start) != 2 {
		t.Errorf("TrimStart across a corner: %v", start)
	}
	if pts[2] != (P{X: 10, Y: 10}) {
		t.Error("TrimEnd modified its input")
	}
}

func TestArrowTipAtEnd(t *testing.T) {
	h := Arrow(draw.ArrowTriangle, P{X: 100, Y: 0}, P{X: 0, Y: 0}, 1.5)
	if h.Triangle[0] != (P{X: 100, Y: 0}) || h.Triangle[1].X >= 100 {
		t.Errorf("triangle %v", h.Triangle)
	}
	c := Arrow(draw.ArrowCircle, P{X: 100, Y: 0}, P{X: 0, Y: 0}, 1.5)
	if c.Center.X+c.Radius > 100.001 {
		t.Errorf("circle %v r %v pokes past the tip", c.Center, c.Radius)
	}
}
