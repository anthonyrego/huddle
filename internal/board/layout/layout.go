// Package layout places a decision tree's cards left to right: one column
// per rank (the longest path from the entry), each card's routes leaving
// from its ports (its response rows) in order, and every route a smooth
// curve from a port to the next card's in-point.
//
// It knows boxes and ports, not text: the editor measures the cards. It is
// built for 30-100 cards laid out again after every edit, so it is simple
// and fast, and it is stable: it starts from the previous layout's order
// and only reorders a column when that removes crossings, so an edit moves
// as little as possible.
//
// Copied from an earlier project of the author's,
// with one root made several (a board has a flow per source).
package layout

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/anthonyrego/huddle/internal/draw"
)

// Node is a card to place.
type Node struct {
	ID    string
	W, H  float32
	In    float32   // the in-point's y from the card's top, on its left edge
	Ports []float32 // each port's y from the card's top, on its right edge
}

// Edge is a route from one of a node's ports to another node.
type Edge struct {
	From string
	Port int // index into the From node's Ports
	To   string
}

// Input is a tree to lay out.
type Input struct {
	Nodes []Node // in file order, which breaks ties
	Edges []Edge
	Roots []string // where the flows start, in order; nodes none of them reach go in a band below
	Prev  *Output  // the previous layout, whose order this keeps where it can
	Style Style
	// Key, when set, orders the cards in every column (a sort someone
	// asked for); routes passing through keep their lanes.
	Key func(a, b string) int
}

// Style is the spacing, in logical units.
type Style struct {
	RankGap float32 // between columns
	NodeGap float32 // between cards in a column
	LaneGap float32 // between a card and a route passing through its column, or two routes
	BandGap float32 // between the main tree and the cards the entry doesn't reach
	Margin  float32 // around everything
}

// DefaultStyle is the editor's spacing.
var DefaultStyle = Style{RankGap: 84, NodeGap: 24, LaneGap: 14, BandGap: 72, Margin: 32}

// Output is a placed tree.
type Output struct {
	W, H   float32
	Boxes  map[string]draw.Rect
	Routes []Route // one per input edge whose ends are both placed, in input order
	Rank   map[string]int
	Band   map[string]int // 0: reachable from the root; 1: not

	center map[string]float32 // the y center of every card and route lane, to seed the next layout
}

// Route is an edge's path.
type Route struct {
	Edge   Edge
	Points []draw.Point // from the port to the in-point, a flattened curve
	// Back is a route that closes a cycle (the spec is broken); Cross runs
	// from the band below into the main tree. Both are drawn as plain
	// curves that ignore the columns.
	Back, Cross bool
}

// Layout places the tree.
func Layout(in Input) *Output {
	st := in.Style
	if st == (Style{}) {
		st = DefaultStyle
	}
	byID := make(map[string]*Node, len(in.Nodes))
	for i := range in.Nodes {
		byID[in.Nodes[i].ID] = &in.Nodes[i]
	}
	var edges []Edge
	for _, e := range in.Edges {
		if byID[e.From] != nil && byID[e.To] != nil && e.Port >= 0 && e.Port < len(byID[e.From].Ports) {
			edges = append(edges, e)
		}
	}

	out := &Output{Boxes: map[string]draw.Rect{}, Rank: map[string]int{}, Band: map[string]int{}, center: map[string]float32{}}
	reach := reachable(in.Roots, byID, edges)
	var bands [2][]*Node
	for i := range in.Nodes {
		n := &in.Nodes[i]
		if reach[n.ID] {
			bands[0] = append(bands[0], n)
		} else {
			bands[1] = append(bands[1], n)
			out.Band[n.ID] = 1
		}
	}

	var prev map[string]float32
	if in.Prev != nil {
		prev = in.Prev.center
	}
	routes := map[int]Route{} // by index into edges
	top := st.Margin
	for b, nodes := range bands {
		if len(nodes) == 0 {
			continue
		}
		var be []int
		for i, e := range edges {
			if out.Band[e.From] == b && out.Band[e.To] == b {
				be = append(be, i)
			}
		}
		var roots []string
		if b == 0 {
			roots = in.Roots
		}
		l := &band{st: st, nodes: nodes, edges: edges, idx: be, roots: roots, prev: prev, key: in.Key}
		h := l.run(st.Margin, top, out, routes)
		top += h + st.BandGap
	}

	for i, e := range edges {
		if _, ok := routes[i]; ok {
			continue
		}
		r := Route{Edge: e, Cross: out.Band[e.From] != out.Band[e.To], Back: out.Band[e.From] == out.Band[e.To]}
		r.Points = direct(portPoint(out.Boxes[e.From], byID[e.From], e.Port), inPoint(out.Boxes[e.To], byID[e.To]))
		routes[i] = r
	}
	for i := range edges {
		out.Routes = append(out.Routes, routes[i])
	}

	for _, r := range out.Boxes {
		out.W = max(out.W, r.Right())
		out.H = max(out.H, r.Bottom())
	}
	for _, r := range out.Routes {
		for _, p := range r.Points {
			out.W = max(out.W, p.X)
			out.H = max(out.H, p.Y)
		}
	}
	out.W += st.Margin
	out.H += st.Margin
	return out
}

func reachable(roots []string, byID map[string]*Node, edges []Edge) map[string]bool {
	out := map[string]bool{}
	next := map[string][]string{}
	for _, e := range edges {
		next[e.From] = append(next[e.From], e.To)
	}
	var stack []string
	for _, r := range roots {
		if byID[r] != nil {
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[id] {
			continue
		}
		out[id] = true
		stack = append(stack, next[id]...)
	}
	return out
}

func portPoint(b draw.Rect, n *Node, port int) draw.Point {
	return draw.Point{X: b.Right(), Y: b.Y + n.Ports[port]}
}

func inPoint(b draw.Rect, n *Node) draw.Point { return draw.Point{X: b.X, Y: b.Y + n.In} }

// unit is a card or a dummy: where a route crosses a column it skips.
type unit struct {
	id    string
	node  *Node // nil for a dummy
	rank  int
	h, in float32
	ports []float32
	y     float32 // top
	pos   int     // index in its column
	ins   []*seg
	outs  []*seg
}

// seg is a route's piece between two adjacent columns.
type seg struct {
	from *unit
	port int
	to   *unit
}

// band lays out one band: the main tree, or what the entry doesn't reach.
type band struct {
	st    Style
	nodes []*Node
	edges []Edge
	idx   []int // the band's edges, as indexes into edges
	roots []string
	prev  map[string]float32
	key   func(a, b string) int

	units map[string]*unit
	cols  [][]*unit
	back  map[int]bool
	chain map[int][]*unit // a forward edge's units, source to target
}

func (l *band) run(x0, y0 float32, out *Output, routes map[int]Route) float32 {
	l.rank()
	l.build()
	l.order()
	l.place()

	// Columns: cards left-aligned at the column's x; a dummy is a lane that
	// runs straight through the column. A gap widens with the steepest
	// drop across it, so long drops stay gentle curves.
	xs := make([]float32, len(l.cols))
	ws := make([]float32, len(l.cols))
	x := x0
	for r, col := range l.cols {
		xs[r] = x
		var drop float32
		for _, u := range col {
			if u.node != nil {
				ws[r] = max(ws[r], u.node.W)
			}
			for _, s := range u.outs {
				drop = max(drop, float32(math.Abs(float64(s.to.y+s.to.in-u.y-u.ports[s.port]))))
			}
		}
		x += ws[r] + min(max(l.st.RankGap, drop*steepness), maxRankGap)
	}
	minY := float32(math.Inf(1))
	for _, col := range l.cols {
		for _, u := range col {
			minY = min(minY, u.y)
		}
	}
	var h float32
	for r, col := range l.cols {
		for _, u := range col {
			u.y += y0 - minY
			out.center[u.id] = u.y + u.h/2
			h = max(h, u.y+u.h-y0)
			if u.node != nil {
				out.Boxes[u.id] = draw.Rect{X: xs[r], Y: u.y, W: u.node.W, H: u.h}
				out.Rank[u.id] = r
			}
		}
	}

	for _, i := range l.idx {
		e := l.edges[i]
		if l.back[i] {
			continue // routed with the cross-band edges, once every box is placed
		}
		ch := l.chain[i]
		src, dst := ch[0], ch[len(ch)-1]
		pts := []draw.Point{portPoint(out.Boxes[src.id], src.node, e.Port)}
		for _, d := range ch[1 : len(ch)-1] {
			pts = curveTo(pts, draw.Point{X: xs[d.rank], Y: d.y})
			pts = append(pts, draw.Point{X: xs[d.rank] + ws[d.rank], Y: d.y})
		}
		pts = curveTo(pts, inPoint(out.Boxes[dst.id], dst.node))
		routes[i] = Route{Edge: e, Points: pts}
	}
	return h
}

// rank gives every card a column: the longest path to it from the band's
// sources, ignoring edges that close a cycle.
func (l *band) rank() {
	l.units = map[string]*unit{}
	for _, n := range l.nodes {
		l.units[n.ID] = &unit{id: n.ID, node: n, h: n.H, in: n.In, ports: n.Ports}
	}
	out := map[string][]int{}
	hasIn := map[string]bool{}
	for _, i := range l.idx {
		e := l.edges[i]
		out[e.From] = append(out[e.From], i)
		hasIn[e.To] = true
	}
	// Depth-first from the roots (or the band's sources), then from
	// whatever is left, in file order: back edges close cycles.
	starts := slices.Clone(l.roots)
	for _, n := range l.nodes {
		if !hasIn[n.ID] {
			starts = append(starts, n.ID)
		}
	}
	for _, n := range l.nodes {
		starts = append(starts, n.ID)
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	l.back = map[int]bool{}
	var post []string
	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		for _, i := range out[id] {
			to := l.edges[i].To
			switch color[to] {
			case white:
				visit(to)
			case grey:
				l.back[i] = true
			}
		}
		color[id] = black
		post = append(post, id)
	}
	for _, id := range starts {
		if l.units[id] != nil && color[id] == white {
			visit(id)
		}
	}
	for k := len(post) - 1; k >= 0; k-- { // topological order
		u := l.units[post[k]]
		for _, i := range out[u.id] {
			if !l.back[i] {
				v := l.units[l.edges[i].To]
				v.rank = max(v.rank, u.rank+1)
			}
		}
	}
}

// build makes the columns: cards, plus a dummy wherever a route crosses a
// column, and the segments between adjacent columns.
func (l *band) build() {
	maxRank := 0
	for _, u := range l.units {
		maxRank = max(maxRank, u.rank)
	}
	l.cols = make([][]*unit, maxRank+1)
	for _, n := range l.nodes {
		u := l.units[n.ID]
		l.cols[u.rank] = append(l.cols[u.rank], u)
	}
	l.chain = map[int][]*unit{}
	for _, i := range l.idx {
		if l.back[i] {
			continue
		}
		e := l.edges[i]
		u, v := l.units[e.From], l.units[e.To]
		ch := []*unit{u}
		for r := u.rank + 1; r < v.rank; r++ {
			d := &unit{id: fmt.Sprintf("%s/%d>%s@%d", e.From, e.Port, e.To, r), rank: r, ports: []float32{0}}
			l.cols[r] = append(l.cols[r], d)
			ch = append(ch, d)
		}
		ch = append(ch, v)
		for k := 0; k+1 < len(ch); k++ {
			port := 0
			if k == 0 {
				port = e.Port
			}
			s := &seg{from: ch[k], port: port, to: ch[k+1]}
			ch[k].outs = append(ch[k].outs, s)
			ch[k+1].ins = append(ch[k+1].ins, s)
		}
		l.chain[i] = ch
	}
}

// srcPos is where a segment leaves its column, in units of positions: a
// port partway down its card, so a card's children order by port.
func srcPos(s *seg) float64 {
	return float64(s.from.pos) + float64(s.port+1)/float64(len(s.from.ports)+1)
}

// order sets each column's order: a first pass down the columns by port
// barycenter (or the previous layout's order where it has one), then
// sweeps up and down that are kept only when they remove crossings.
func (l *band) order() {
	// Column 0 in the walk's order: roots first, then file order.
	first := l.cols[0]
	rootAt := func(u *unit) int {
		if i := slices.Index(l.roots, u.id); i >= 0 {
			return i
		}
		return len(l.roots)
	}
	slices.SortStableFunc(first, func(a, b *unit) int { return cmp.Compare(rootAt(a), rootAt(b)) })
	l.reindex(0)
	for r := 1; r < len(l.cols); r++ {
		l.sortBy(r, func(u *unit) (float64, bool) { return l.inBary(u) })
	}
	if l.prev != nil {
		l.seed()
	}

	best := l.crossings()
	for it := 0; it < 8 && best > 0; it++ {
		snap := l.snapshot()
		if it%2 == 0 {
			for r := 1; r < len(l.cols); r++ {
				l.sortBy(r, func(u *unit) (float64, bool) { return l.inBary(u) })
			}
		} else {
			for r := len(l.cols) - 2; r >= 0; r-- {
				l.sortBy(r, func(u *unit) (float64, bool) { return l.outBary(u) })
			}
		}
		if c := l.crossings(); c < best {
			best = c
		} else {
			l.restore(snap)
		}
	}
	if l.key != nil {
		l.sortByKey()
	}
}

// sortByKey puts each column's cards in key order, in the slots cards
// already hold: lanes stay where they are.
func (l *band) sortByKey() {
	for r, col := range l.cols {
		var slots []int
		var cards []*unit
		for i, u := range col {
			if u.node != nil {
				slots = append(slots, i)
				cards = append(cards, u)
			}
		}
		slices.SortStableFunc(cards, func(a, b *unit) int { return l.key(a.id, b.id) })
		for k, i := range slots {
			col[i] = cards[k]
		}
		l.reindex(r)
	}
}

// Seed is a previous layout made only of where things were: the y
// centers of cards (and lanes) by id. A layout seeded from it keeps their
// order where it can.
func Seed(centers map[string]float32) *Output { return &Output{center: centers} }

// Centers are the y centers of everything placed, to seed the next layout.
func (o *Output) Centers() map[string]float32 { return o.center }

// Curve is a route that ignores columns: out of p to the right, into q
// from the left.
func Curve(p, q draw.Point) []draw.Point { return direct(p, q) }

func (l *band) inBary(u *unit) (float64, bool) {
	if len(u.ins) == 0 {
		return 0, false
	}
	var s float64
	for _, sg := range u.ins {
		s += srcPos(sg)
	}
	return s / float64(len(u.ins)), true
}

func (l *band) outBary(u *unit) (float64, bool) {
	if len(u.outs) == 0 {
		return 0, false
	}
	var s float64
	for _, sg := range u.outs {
		// Line the port up with its target: a lower port pulls the card up.
		s += float64(sg.to.pos) + 0.5 - float64(sg.port+1)/float64(len(u.ports)+1)
	}
	return s / float64(len(u.outs)), true
}

// sortBy reorders a column by key; a unit without one keeps its place.
func (l *band) sortBy(r int, key func(*unit) (float64, bool)) {
	col := l.cols[r]
	keys := make(map[*unit]float64, len(col))
	for i, u := range col {
		k, ok := key(u)
		if !ok {
			k = float64(i)
		}
		keys[u] = k
	}
	slices.SortStableFunc(col, func(a, b *unit) int { return cmp.Compare(keys[a], keys[b]) })
	l.reindex(r)
}

// seed reorders each column by where its units were in the previous
// layout; a new unit goes right after the unit it followed in the fresh
// order.
func (l *band) seed() {
	for r, col := range l.cols {
		keys := make(map[*unit]float64, len(col))
		last, n := math.Inf(-1), 0
		for _, u := range col {
			if y, ok := l.prev[u.id]; ok {
				keys[u] = float64(y)
				last, n = float64(y), 0
				continue
			}
			n++
			keys[u] = last + 1e-3*float64(n)
		}
		slices.SortStableFunc(col, func(a, b *unit) int { return cmp.Compare(keys[a], keys[b]) })
		l.reindex(r)
	}
}

func (l *band) reindex(r int) {
	for i, u := range l.cols[r] {
		u.pos = i
	}
}

func (l *band) snapshot() [][]*unit {
	s := make([][]*unit, len(l.cols))
	for r, col := range l.cols {
		s[r] = slices.Clone(col)
	}
	return s
}

func (l *band) restore(s [][]*unit) {
	l.cols = s
	for r := range l.cols {
		l.reindex(r)
	}
}

// crossings counts segment pairs that cross between adjacent columns.
func (l *band) crossings() int {
	n := 0
	for _, col := range l.cols {
		var segs []*seg
		for _, u := range col {
			segs = append(segs, u.outs...)
		}
		for i := range segs {
			a, as := segs[i], srcPos(segs[i])
			for _, b := range segs[i+1:] {
				bs := srcPos(b)
				if as < bs && a.to.pos > b.to.pos || as > bs && a.to.pos < b.to.pos {
					n++
				}
			}
		}
	}
	return n
}

// place sets each unit's top: down the columns lining in-points up with
// the ports that lead to them, back up lining ports up with their
// targets, and down again, each column packed without overlaps as close
// to those targets as it can be.
func (l *band) place() {
	for r := range l.cols {
		l.pack(r, func(u *unit) (float32, bool) { return 0, false })
	}
	down := func() {
		for r := 1; r < len(l.cols); r++ {
			l.pack(r, func(u *unit) (float32, bool) {
				var ys []float32
				for _, s := range u.ins {
					ys = append(ys, s.from.y+s.from.ports[s.port]-u.in)
				}
				return median(ys)
			})
		}
	}
	up := func() {
		for r := len(l.cols) - 2; r >= 0; r-- {
			l.pack(r, func(u *unit) (float32, bool) {
				var ys []float32
				for _, s := range u.outs {
					ys = append(ys, s.to.y+s.to.in-u.ports[s.port])
				}
				return median(ys)
			})
		}
	}
	down()
	up()
	down()
}

func median(ys []float32) (float32, bool) {
	switch len(ys) {
	case 0:
		return 0, false
	case 1:
		return ys[0], true
	}
	slices.Sort(ys)
	m := len(ys) / 2
	if len(ys)%2 == 1 {
		return ys[m], true
	}
	return (ys[m-1] + ys[m]) / 2, true
}

// pack places a column's units in order, each as close to its target top
// as the units before it allow, minimizing the total squared distance
// from the targets: with z = y - (the space everything above needs), the
// gaps become z non-decreasing, which is isotonic regression (pool
// adjacent violators). A unit with no target keeps its top.
func (l *band) pack(r int, target func(*unit) (float32, bool)) {
	col := l.cols[r]
	n := len(col)
	if n == 0 {
		return
	}
	offs := make([]float64, n) // space needed above each unit
	for i := 1; i < n; i++ {
		offs[i] = offs[i-1] + float64(col[i-1].h+l.gap(col[i-1], col[i]))
	}
	type block struct {
		sum   float64
		count int
		end   int
	}
	var blocks []block
	for i, u := range col {
		t, ok := target(u)
		if !ok {
			t = u.y
		}
		b := block{sum: float64(t) - offs[i], count: 1, end: i}
		for len(blocks) > 0 && blocks[len(blocks)-1].sum/float64(blocks[len(blocks)-1].count) > b.sum/float64(b.count) {
			p := blocks[len(blocks)-1]
			blocks = blocks[:len(blocks)-1]
			b = block{sum: p.sum + b.sum, count: p.count + b.count, end: b.end}
		}
		blocks = append(blocks, b)
	}
	i := 0
	for _, b := range blocks {
		z := b.sum / float64(b.count)
		for ; i <= b.end; i++ {
			col[i].y = float32(z + offs[i])
		}
	}
}

func (l *band) gap(a, b *unit) float32 {
	if a.node != nil && b.node != nil {
		return l.st.NodeGap
	}
	return l.st.LaneGap
}

// A gap between columns is at least steepness times the largest vertical
// drop across it, up to maxRankGap.
const (
	steepness  = 0.3
	maxRankGap = 200
)

// curveSteps is how many segments a route's curve between two columns
// is flattened into.
const curveSteps = 16

// curveTo appends a curve from the last point to q, leaving and arriving
// horizontally.
func curveTo(pts []draw.Point, q draw.Point) []draw.Point {
	p := pts[len(pts)-1]
	dx := max((q.X-p.X)/2, 24)
	return cubic(pts, p, draw.Point{X: p.X + dx, Y: p.Y}, draw.Point{X: q.X - dx, Y: q.Y}, q)
}

// direct is a route that ignores the columns: out of the port to the
// right, into the in-point from the left.
func direct(p, q draw.Point) []draw.Point {
	d := max(float32(math.Abs(float64(q.X-p.X)))/2, 48)
	return cubic([]draw.Point{p}, p, draw.Point{X: p.X + d, Y: p.Y}, draw.Point{X: q.X - d, Y: q.Y}, q)
}

func cubic(pts []draw.Point, p0, c1, c2, p1 draw.Point) []draw.Point {
	for i := 1; i <= curveSteps; i++ {
		t := float32(i) / curveSteps
		u := 1 - t
		a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		pts = append(pts, draw.Point{
			X: a*p0.X + b*c1.X + c*c2.X + d*p1.X,
			Y: a*p0.Y + b*c1.Y + c*c2.Y + d*p1.Y,
		})
	}
	return pts
}
