package gfx

import (
	"math"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/geom"
)

func pathBatch(p draw.Path, scale float32) *Batch {
	b := &Batch{}
	b.Reset(2000, 2000)
	var l draw.List
	l.Path(p)
	b.AddList(&l, scale)
	return b
}

func kinds(b *Batch) map[Kind]int {
	m := map[Kind]int{}
	for _, in := range b.Instances {
		m[Kind(in.Params[2])]++
	}
	return m
}

var ink = draw.RGB(0x333333)

func TestPolylineSegmentsShareJoints(t *testing.T) {
	pts := []draw.Point{{X: 10, Y: 10}, {X: 50, Y: 10}, {X: 50, Y: 60}, {X: 90, Y: 60}}
	b := pathBatch(draw.Path{Points: pts, Stroke: ink, Width: 2}, 2)
	if len(b.Instances) != 3 {
		t.Fatalf("instances = %d, want one per segment", len(b.Instances))
	}
	for i, in := range b.Instances {
		if Kind(in.Params[2]) != KindStroke || in.Params[0] != 4 {
			t.Fatalf("segment %d: kind %v width %v", i, in.Params[2], in.Params[0])
		}
		a := draw.Point{X: in.Radii[0], Y: in.Radii[1]}
		c := draw.Point{X: in.Radii[2], Y: in.Radii[3]}
		if a != (draw.Point{X: pts[i].X * 2, Y: pts[i].Y * 2}) || c != (draw.Point{X: pts[i+1].X * 2, Y: pts[i+1].Y * 2}) {
			t.Fatalf("segment %d runs %v-%v", i, a, c)
		}
		// The quad covers the capsule.
		if in.Rect[0] > min(a.X, c.X)-2 || in.Rect[1] > min(a.Y, c.Y)-2 || in.Rect[0]+in.Rect[2] < max(a.X, c.X)+2 {
			t.Fatalf("segment %d quad %v doesn't cover it", i, in.Rect)
		}
	}
	// Free ends have no tangent; a joint's tangent is the same on both sides.
	first, last := b.Instances[0], b.Instances[2]
	if first.UV[0] != 0 || first.UV[1] != 0 || last.UV[2] != 0 || last.UV[3] != 0 {
		t.Fatalf("free ends carry tangents: %v %v", first.UV, last.UV)
	}
	for i := 0; i < 2; i++ {
		out, in := b.Instances[i].UV, b.Instances[i+1].UV
		if out[2] != in[0] || out[3] != in[1] {
			t.Fatalf("joint %d: tangents %v and %v differ", i+1, out[2:], in[:2])
		}
		if l := math.Hypot(float64(in[0]), float64(in[1])); math.Abs(l-1) > 1e-5 {
			t.Fatalf("joint tangent not unit: %v", in[:2])
		}
	}
	// A right turn from +x to +y: the tangent bisects them.
	if tx, ty := b.Instances[0].UV[2], b.Instances[0].UV[3]; math.Abs(float64(tx-ty)) > 1e-6 || tx <= 0 {
		t.Fatalf("tangent at the first corner = (%v, %v)", tx, ty)
	}
}

func TestClosedRingJoinsAllRound(t *testing.T) {
	sq := []draw.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
	b := pathBatch(draw.Path{Points: sq, Closed: true, Stroke: ink, Fill: draw.RGB(0xffffff), Width: 1}, 1)
	k := kinds(b)
	if k[KindTriangle] != 2 || k[KindStroke] != 4 {
		t.Fatalf("kinds %v, want 2 triangles and 4 segments", k)
	}
	for _, in := range b.Instances {
		if Kind(in.Params[2]) == KindStroke && (in.UV == [4]float32{} || in.UV[2] == 0 && in.UV[3] == 0) {
			t.Fatalf("closed ring has a free end: %v", in.UV)
		}
	}
}

func TestFanMarksOutlineEdges(t *testing.T) {
	hex := []draw.Point{{X: 7.5, Y: 0}, {X: 52.5, Y: 0}, {X: 60, Y: 15}, {X: 52.5, Y: 30}, {X: 7.5, Y: 30}, {X: 0, Y: 15}}
	b := pathBatch(draw.Path{Points: hex, Closed: true, Fill: ink}, 1)
	if len(b.Instances) != 4 {
		t.Fatalf("hexagon: %d triangles, want 4", len(b.Instances))
	}
	outline := 0
	for _, in := range b.Instances {
		for _, e := range in.BorderColor[:3] {
			if e == 255 {
				outline++
			}
		}
	}
	if outline != 6 {
		t.Fatalf("%d edges flagged as outline, want the hexagon's 6", outline)
	}
}

func TestConcaveFill(t *testing.T) {
	// A flag: the notch on the left makes it concave.
	flag := []draw.Point{{X: 0, Y: 0}, {X: 80, Y: 0}, {X: 80, Y: 40}, {X: 0, Y: 40}, {X: 10, Y: 20}}
	b := pathBatch(draw.Path{Points: flag, Closed: true, Fill: ink}, 1)
	if len(b.Instances) != 3 {
		t.Fatalf("flag: %d triangles, want 3", len(b.Instances))
	}
	var area float64
	outline := 0
	for _, in := range b.Instances {
		v0 := draw.Point{X: in.Radii[0], Y: in.Radii[1]}
		v1 := draw.Point{X: in.Radii[2], Y: in.Radii[3]}
		v2 := draw.Point{X: in.UV[0], Y: in.UV[1]}
		area += math.Abs(float64((v1.X-v0.X)*(v2.Y-v0.Y)-(v1.Y-v0.Y)*(v2.X-v0.X))) / 2
		for _, e := range in.BorderColor[:3] {
			if e == 255 {
				outline++
			}
		}
	}
	// 80×40 minus the notch triangle (base 40, depth 10).
	if want := 80.0*40 - 40*10/2.0; math.Abs(area-want) > 0.01 {
		t.Fatalf("triangles cover %.1f, want %.1f", area, want)
	}
	if outline != 5 {
		t.Fatalf("%d outline edges, want 5", outline)
	}
}

func TestDashesAreButtEndedPieces(t *testing.T) {
	w := float32(1.5)
	on, off := geom.Dash(w)
	length := 5*(on+off) + on/2
	b := pathBatch(draw.Path{Points: []draw.Point{{X: 0, Y: 5}, {X: length, Y: 5}}, Stroke: ink, Width: w, Dashed: true}, 1)
	if len(b.Instances) != 6 {
		t.Fatalf("%d dashes, want 6", len(b.Instances))
	}
	for i, in := range b.Instances {
		if in.BorderColor[0] != 255 || in.BorderColor[1] != 255 {
			t.Fatalf("dash %d isn't butt-capped", i)
		}
		x0 := float32(i) * (on + off)
		if math.Abs(float64(in.Radii[0]-x0)) > 1e-3 {
			t.Fatalf("dash %d starts at %v, want %v", i, in.Radii[0], x0)
		}
	}
}

func TestArrowheadsFollowGeom(t *testing.T) {
	pts := []draw.Point{{X: 0, Y: 0}, {X: 100, Y: 0}}
	w := float32(1.5)
	b := pathBatch(draw.Path{Points: pts, Stroke: ink, Width: w, ArrowEnd: draw.ArrowTriangle, ArrowStart: draw.ArrowCircle}, 1)
	k := kinds(b)
	if k[KindStroke] != 1 || k[KindTriangle] != 1 || k[KindShape] != 1 {
		t.Fatalf("kinds %v", k)
	}
	seg := b.Instances[0]
	if want := 100 - geom.EndTrim(draw.ArrowTriangle, w); math.Abs(float64(seg.Radii[2]-want)) > 1e-3 {
		t.Fatalf("stroke ends at %v, want %v (short of the tip)", seg.Radii[2], want)
	}
	if want := geom.EndTrim(draw.ArrowCircle, w); math.Abs(float64(seg.Radii[0]-want)) > 1e-3 {
		t.Fatalf("stroke starts at %v, want %v", seg.Radii[0], want)
	}
	for _, in := range b.Instances {
		if Kind(in.Params[2]) == KindTriangle {
			tip := geom.Arrow(draw.ArrowTriangle, pts[1], pts[0], w).Triangle[0]
			if in.Radii[0] != tip.X || in.Radii[1] != tip.Y {
				t.Fatalf("triangle starts at %v, want the tip %v", in.Radii[:2], tip)
			}
		}
	}
	x := pathBatch(draw.Path{Points: pts, Stroke: ink, Width: w, ArrowEnd: draw.ArrowCross}, 1)
	if k := kinds(x); k[KindStroke] != 3 {
		t.Fatalf("cross: kinds %v, want the line and two strokes", k)
	}
}

func TestPathsOutsideTheClipAreCulled(t *testing.T) {
	b := &Batch{}
	b.Reset(100, 100)
	var l draw.List
	l.Path(draw.Path{Points: []draw.Point{{X: 500, Y: 500}, {X: 600, Y: 600}}, Stroke: ink, Width: 2, ArrowEnd: draw.ArrowTriangle})
	// A long line crossing the view keeps only its visible segments.
	l.Path(draw.Path{Points: []draw.Point{{X: -500, Y: 50}, {X: 50, Y: 50}, {X: 700, Y: 50}, {X: 700, Y: 700}}, Stroke: ink, Width: 2})
	b.AddList(&l, 1)
	if len(b.Instances) != 2 {
		t.Fatalf("%d instances, want the 2 segments that cross the view", len(b.Instances))
	}
}

func TestPathTessellationDoesNotAllocate(t *testing.T) {
	b := &Batch{}
	var l draw.List
	curve := geom.Basis([]draw.Point{{X: 0, Y: 0}, {X: 40, Y: 80}, {X: 120, Y: 40}, {X: 200, Y: 200}}, 8)
	l.Path(draw.Path{Points: curve, Stroke: ink, Width: 1.5, Dashed: true, ArrowEnd: draw.ArrowTriangle})
	l.Path(draw.Path{Points: geom.Arc(50, 35, 40, 25, 0, 2*math.Pi, 48), Closed: true, Fill: ink, Stroke: ink, Width: 1})
	l.Path(draw.Path{Points: []draw.Point{{X: 10, Y: 10}, {X: 90, Y: 10}, {X: 90, Y: 60}, {X: 10, Y: 60}, {X: 22.5, Y: 35}}, Closed: true, Fill: ink, Stroke: ink, Width: 1})
	frame := func() {
		b.Reset(1000, 1000)
		b.AddList(&l, 2)
	}
	frame()
	if n := testing.AllocsPerRun(20, frame); n > 0 {
		t.Fatalf("%v allocations per frame", n)
	}
}
