package layout

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
)

// card makes a node with n ports, as the editor's cards are: a header, then
// one row per port.
func card(id string, ports int) Node {
	n := Node{ID: id, W: 240, H: 56 + 24*float32(ports), In: 14}
	for i := range ports {
		n.Ports = append(n.Ports, 56+24*float32(i)+12)
	}
	return n
}

func TestChildrenFollowPortOrder(t *testing.T) {
	in := Input{
		Roots: []string{"a"},
		Nodes: []Node{card("a", 3), card("z", 0), card("y", 0), card("x", 0)},
		// Ports in order lead to x, y, z; file order is the reverse.
		Edges: []Edge{{"a", 0, "x"}, {"a", 1, "y"}, {"a", 2, "z"}},
	}
	out := Layout(in)
	if !(out.Boxes["x"].Y < out.Boxes["y"].Y && out.Boxes["y"].Y < out.Boxes["z"].Y) {
		t.Fatalf("children not in port order: x %v y %v z %v", out.Boxes["x"], out.Boxes["y"], out.Boxes["z"])
	}
	for _, id := range []string{"x", "y", "z"} {
		if out.Rank[id] != 1 || out.Boxes[id].X <= out.Boxes["a"].Right() {
			t.Fatalf("%s: rank %d at %v", id, out.Rank[id], out.Boxes[id])
		}
	}
	checkRoutes(t, in, out)
}

func TestSharedNodeRanksAfterItsDeepestParent(t *testing.T) {
	in := Input{
		Roots: []string{"a"},
		Nodes: []Node{card("a", 2), card("b", 1), card("c", 1), card("end", 0)},
		Edges: []Edge{{"a", 0, "b"}, {"a", 1, "end"}, {"b", 0, "c"}, {"c", 0, "end"}},
	}
	out := Layout(in)
	if out.Rank["end"] != 3 {
		t.Fatalf("end at rank %d, want 3", out.Rank["end"])
	}
	// a -> end crosses columns 1 and 2: its route passes through them.
	r := out.Routes[1]
	if r.Edge.To != "end" || r.Back || r.Cross || len(r.Points) < 3*curveSteps {
		t.Fatalf("a->end route: %+v (%d points)", r.Edge, len(r.Points))
	}
	checkRoutes(t, in, out)
}

func TestUnreachedNodesGoInABandBelow(t *testing.T) {
	in := Input{
		Roots: []string{"a"},
		Nodes: []Node{card("a", 1), card("b", 0), card("lost", 1)},
		Edges: []Edge{{"a", 0, "b"}, {"lost", 0, "b"}},
	}
	out := Layout(in)
	if out.Band["lost"] != 1 || out.Boxes["lost"].Y <= out.Boxes["b"].Bottom() {
		t.Fatalf("lost: band %d at %v; main tree ends at %v", out.Band["lost"], out.Boxes["lost"], out.Boxes["b"].Bottom())
	}
	if !out.Routes[1].Cross {
		t.Fatal("lost -> b should be a cross-band route")
	}
}

func TestCycleIsDrawnNotFollowed(t *testing.T) {
	in := Input{
		Roots: []string{"a"},
		Nodes: []Node{card("a", 1), card("b", 1)},
		Edges: []Edge{{"a", 0, "b"}, {"b", 0, "a"}},
	}
	out := Layout(in)
	if out.Rank["a"] != 0 || out.Rank["b"] != 1 || !out.Routes[1].Back {
		t.Fatalf("ranks a %d b %d, back %v", out.Rank["a"], out.Rank["b"], out.Routes[1].Back)
	}
}

func TestRandomTreesDoNotOverlap(t *testing.T) {
	for seed := range uint64(200) {
		in := randomTree(seed, 30+int(seed%71))
		out := Layout(in)
		if len(out.Boxes) != len(in.Nodes) {
			t.Fatalf("seed %d: placed %d of %d", seed, len(out.Boxes), len(in.Nodes))
		}
		ids := make([]string, 0, len(out.Boxes))
		for id := range out.Boxes {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for i, a := range ids {
			for _, b := range ids[i+1:] {
				if overlap(out.Boxes[a], out.Boxes[b]) {
					t.Fatalf("seed %d: %s %v overlaps %s %v", seed, a, out.Boxes[a], b, out.Boxes[b])
				}
			}
		}
		checkRoutes(t, in, out)
	}
}

func TestTextEditKeepsEveryCardInItsColumnAndOrder(t *testing.T) {
	for seed := range uint64(50) {
		in := randomTree(seed, 60)
		before := Layout(in)
		in.Nodes[7].H += 18 // a title wrapped onto a second line
		in.Prev = before
		after := Layout(in)
		if c1, c2 := columns(before), columns(after); !slices.EqualFunc(c1, c2, slices.Equal) {
			t.Fatalf("seed %d: columns changed\nbefore %v\nafter  %v", seed, c1, c2)
		}
	}
}

func TestAddingALeafKeepsTheOtherCardsInOrder(t *testing.T) {
	for seed := range uint64(50) {
		in := randomTree(seed, 60)
		before := Layout(in)
		// A new response on node 3 leading to a new leaf.
		n := &in.Nodes[3]
		n.Ports = append(n.Ports, n.H+12)
		n.H += 24
		in.Nodes = append(in.Nodes, card("new", 0))
		in.Edges = append(in.Edges, Edge{n.ID, len(n.Ports) - 1, "new"})
		in.Prev = before
		after := columns(Layout(in))
		for r, col := range columns(before) {
			got := slices.DeleteFunc(slices.Clone(after[r]), func(id string) bool { return id == "new" })
			if !slices.Equal(got, col) {
				t.Fatalf("seed %d: column %d reordered\nbefore %v\nafter  %v", seed, r, col, after[r])
			}
		}
	}
}

func BenchmarkLayout100(b *testing.B) {
	in := randomTree(1, 100)
	prev := Layout(in)
	in.Prev = prev
	for b.Loop() {
		Layout(in)
	}
}

func TestLayout100IsFast(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	r := testing.Benchmark(BenchmarkLayout100)
	if per := r.NsPerOp(); per > 5e6 {
		t.Fatalf("layout of 100 cards takes %.2f ms, want under 5", float64(per)/1e6)
	}
}

// randomTree is a decision tree's shape: a random tree (each card hangs off
// a port of an earlier one) plus a few routes into shared subtrees.
func randomTree(seed uint64, n int) Input {
	rng := rand.New(rand.NewPCG(seed, 11))
	ports := make([]int, n)
	type link struct{ from, to int }
	var links []link
	for j := 1; j < n; j++ {
		p := max(0, j-1-rng.IntN(min(j, 6)))
		links = append(links, link{p, j})
		ports[p]++
	}
	for range n / 8 {
		i := rng.IntN(n - 1)
		j := i + 1 + rng.IntN(n-1-i)
		links = append(links, link{i, j})
		ports[i]++
	}
	in := Input{Roots: []string{"n0"}}
	for i := range n {
		in.Nodes = append(in.Nodes, card(fmt.Sprintf("n%d", i), ports[i]))
	}
	next := make([]int, n)
	for _, l := range links {
		in.Edges = append(in.Edges, Edge{fmt.Sprintf("n%d", l.from), next[l.from], fmt.Sprintf("n%d", l.to)})
		next[l.from]++
	}
	return in
}

func columns(out *Output) [][]string {
	var cols [][]string
	for id, r := range out.Rank {
		for len(cols) <= r {
			cols = append(cols, nil)
		}
		cols[r] = append(cols[r], id)
	}
	for _, col := range cols {
		slices.SortFunc(col, func(a, b string) int {
			if out.Boxes[a].Y < out.Boxes[b].Y {
				return -1
			}
			return 1
		})
	}
	return cols
}

func overlap(a, b draw.Rect) bool {
	return a.X < b.Right() && b.X < a.Right() && a.Y < b.Bottom() && b.Y < a.Bottom()
}

// checkRoutes checks every route starts at its port and ends at its
// target's in-point.
func checkRoutes(t *testing.T, in Input, out *Output) {
	t.Helper()
	nodes := map[string]Node{}
	for _, n := range in.Nodes {
		nodes[n.ID] = n
	}
	near := func(a, b draw.Point) bool { return abs(a.X-b.X) < 0.01 && abs(a.Y-b.Y) < 0.01 }
	for _, r := range out.Routes {
		from, to := out.Boxes[r.Edge.From], out.Boxes[r.Edge.To]
		start := draw.Point{X: from.Right(), Y: from.Y + nodes[r.Edge.From].Ports[r.Edge.Port]}
		end := draw.Point{X: to.X, Y: to.Y + nodes[r.Edge.To].In}
		if !near(r.Points[0], start) || !near(r.Points[len(r.Points)-1], end) {
			t.Fatalf("route %+v runs %v..%v, want %v..%v", r.Edge, r.Points[0], r.Points[len(r.Points)-1], start, end)
		}
	}
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
