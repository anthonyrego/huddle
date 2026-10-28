package layout

import (
	"math"
	"slices"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/draw"
)

// The whole board. Each group is laid out on its own, as a flow when its
// cards have arrows between them and as a list (in columns when it is
// long) when they don't, and becomes one block: a group's cards stay
// together and its frame covers nothing else. The blocks and the cards in
// no group are then laid out as flows, one per part of the board that
// arrows join, and the parts are packed in rows, in the order they came
// up (or the board's sort). The notes panel goes to the right.
//
// The server runs this and sends the Placement with each version; an app
// that has no placement (a fixture, an old version) runs the same code, so
// every picture of a version is the same picture.

// Box is a rectangle in a placement.
type Box struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	W float32 `json:"w"`
	H float32 `json:"h"`
}

// Rect is the box as a draw.Rect.
func (b Box) Rect() draw.Rect { return draw.Rect{X: b.X, Y: b.Y, W: b.W, H: b.H} }

// Placement is where everything on a board goes, in board units.
type Placement struct {
	W float32 `json:"w"`
	H float32 `json:"h"`
	// Boxes are the cards that don't hang from another, each with its
	// satellites (its height is the card's and theirs).
	Boxes  map[string]Box `json:"boxes"`
	Groups map[string]Box `json:"groups,omitempty"` // frames, with their label band
	// Arrows are the arrows, in the board's link order.
	Arrows []Arrow `json:"arrows,omitempty"`
	Notes  *Box    `json:"notes,omitempty"`
	// Centers are the y centers of cards, frames and lanes, which seed the
	// next layout so an add moves as little as possible.
	Centers map[string]float32 `json:"centers,omitempty"`
}

// Arrow is an arrow's path in a placement.
type Arrow struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	Pts  []float32 `json:"pts"` // x0, y0, x1, y1, …
}

// Points is the path as points.
func (r Arrow) Points() []draw.Point {
	pts := make([]draw.Point, len(r.Pts)/2)
	for i := range pts {
		pts[i] = draw.Point{X: r.Pts[2*i], Y: r.Pts[2*i+1]}
	}
	return pts
}

// Sizer measures what the layout places.
type Sizer interface {
	// Card is a card's width, its own height, and its height with its
	// satellites.
	Card(n *board.Node, sats []*board.Node) (w, h, totalH float32)
	// Notes is the notes panel's size.
	Notes(lines []string) (w, h float32)
	// GroupLabel is the width of a frame's label.
	GroupLabel(label string) float32
}

// BoardStyle is the board's spacing, in board units.
type BoardStyle struct {
	Flow        Style   // within a flow (its Margin is unused)
	Margin      float32 // around everything
	GroupPad    float32 // between a frame and its cards
	GroupLabelH float32 // the frame's label band, above its cards
	ListGap     float32 // between cards in a list
	ListColGap  float32 // between a long list's columns
	MaxList     int     // a list of more cards than this wraps into columns
	PackGap     float32 // between parts of the board no arrow joins
	Aspect      float32 // the width over height the parts are packed to
	NotesGap    float32 // between the board and the notes panel
	EmptyGroupW float32 // a frame with nothing in it yet
	EmptyGroupH float32
}

// DefaultBoardStyle is the board's spacing.
var DefaultBoardStyle = BoardStyle{
	Flow:   Style{RankGap: 72, NodeGap: 44, LaneGap: 16, BandGap: 64},
	Margin: 48, GroupPad: 14, GroupLabelH: 22, ListGap: 16, ListColGap: 24, MaxList: 7,
	PackGap: 56, Aspect: 1.6, NotesGap: 56, EmptyGroupW: 180, EmptyGroupH: 44,
}

// Board places a board, seeded from prev (nil for none).
func Board(doc *board.Doc, sz Sizer, prev *Placement) *Placement {
	return BoardWith(doc, sz, prev, DefaultBoardStyle)
}

type cardSize struct{ w, h, total float32 }

// fnode is a card or a frame in a flow.
type fnode struct {
	id       string
	w, h     float32
	in, port float32
}

// part is something laid out on its own: a group's inside, or a part of
// the top level. Positions are from its top-left.
type part struct {
	w, h    float32
	pos     map[string]draw.Point
	routes  map[[2]string][]draw.Point
	centers map[string]float32
}

func (p *part) shift(dx, dy float32) {
	for id, q := range p.pos {
		p.pos[id] = draw.Point{X: q.X + dx, Y: q.Y + dy}
	}
	for k, pts := range p.routes {
		for i := range pts {
			pts[i].X += dx
			pts[i].Y += dy
		}
		p.routes[k] = pts
	}
	for id, y := range p.centers {
		p.centers[id] = y + dy
	}
}

// flow lays out nodes joined by edges left to right.
func flow(nodes []fnode, edges []Edge, key func(a, b string) int, seed *Output, st Style) *part {
	in := Input{Edges: edges, Prev: seed, Style: st, Key: key}
	in.Style.Margin = 0
	hasIn := map[string]bool{}
	for _, e := range edges {
		hasIn[e.To] = true
	}
	for _, n := range nodes {
		in.Nodes = append(in.Nodes, Node{ID: n.id, W: n.w, H: n.h, In: n.in, Ports: []float32{n.port}})
		if !hasIn[n.id] {
			in.Roots = append(in.Roots, n.id)
		}
	}
	out := Layout(in)
	p := &part{w: out.W, h: out.H, pos: map[string]draw.Point{}, routes: map[[2]string][]draw.Point{}, centers: map[string]float32{}}
	for _, n := range nodes {
		r := out.Boxes[n.id]
		p.pos[n.id] = draw.Point{X: r.X, Y: r.Y}
	}
	for _, r := range out.Routes {
		p.routes[[2]string{r.Edge.From, r.Edge.To}] = r.Points
	}
	for id, y := range out.Centers() {
		p.centers[id] = y
	}
	return p
}

// BoardWith places a board with the given spacing.
func BoardWith(doc *board.Doc, sz Sizer, prev *Placement, st BoardStyle) *Placement {
	var seed *Output
	if prev != nil && len(prev.Centers) > 0 {
		seed = Seed(prev.Centers)
	}
	pl := &Placement{Boxes: map[string]Box{}, Groups: map[string]Box{}, Centers: map[string]float32{}}

	cards := map[string]cardSize{}
	var top []*board.Node
	for _, n := range doc.Nodes() {
		if n.On != "" {
			continue
		}
		w, h, t := sz.Card(n, doc.Satellites(n.ID))
		cards[n.ID] = cardSize{w, h, t}
		top = append(top, n)
	}
	groupOf := func(id string) string {
		n := doc.Node(id)
		if n == nil || n.On != "" || n.Group == "" || doc.Group(n.Group) == nil {
			return ""
		}
		return n.Group
	}
	links := doc.Links()
	cardNode := func(id string) fnode {
		c := cards[id]
		return fnode{id: id, w: c.w, h: c.total, in: c.h / 2, port: c.h / 2}
	}

	// Each group's inside.
	blocks := map[string]*part{}
	for _, g := range doc.Groups() {
		var members []string
		for _, n := range top {
			if groupOf(n.ID) == g.ID {
				members = append(members, n.ID)
			}
		}
		blocks[g.ID] = layoutGroup(g, members, links, cards, cardNode, doc.Compare(g.ID), seed, sz, st, groupOf)
	}

	// The top level: frames and the cards in no group, in the order they
	// came up.
	unitOf := func(id string) string {
		if g := groupOf(id); g != "" {
			return g
		}
		return id
	}
	var units []fnode
	seen := map[string]bool{}
	addGroup := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		b := blocks[id]
		units = append(units, fnode{id: id, w: b.w, h: b.h, in: b.h / 2, port: b.h / 2})
	}
	for _, n := range top {
		if g := groupOf(n.ID); g != "" {
			addGroup(g)
			continue
		}
		seen[n.ID] = true
		units = append(units, cardNode(n.ID))
	}
	for _, g := range doc.Groups() {
		addGroup(g.ID)
	}
	var edges []Edge
	seenEdge := map[[2]string]bool{}
	for _, l := range links {
		a, b := unitOf(l.From), unitOf(l.To)
		if a == b || seenEdge[[2]string{a, b}] {
			continue
		}
		seenEdge[[2]string{a, b}] = true
		edges = append(edges, Edge{From: a, To: b})
	}

	// Parts of the board that arrows join.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		return x
	}
	for _, u := range units {
		parent[u.id] = u.id
	}
	for _, e := range edges {
		if ra, rb := find(e.From), find(e.To); ra != rb {
			parent[rb] = ra
		}
	}
	var comps [][]fnode
	compOf := map[string]int{}
	for _, u := range units {
		r := find(u.id)
		i, ok := compOf[r]
		if !ok {
			i = len(comps)
			compOf[r] = i
			comps = append(comps, nil)
		}
		comps[i] = append(comps[i], u)
	}
	boardKey := doc.Compare("")
	if boardKey != nil {
		first := func(c []fnode) string {
			best := c[0].id
			for _, u := range c[1:] {
				if boardKey(u.id, best) < 0 {
					best = u.id
				}
			}
			return best
		}
		slices.SortStableFunc(comps, func(a, b []fnode) int { return boardKey(first(a), first(b)) })
	}
	parts := make([]*part, len(comps))
	for i, c := range comps {
		if len(c) == 1 {
			parts[i] = &part{w: c[0].w, h: c[0].h, pos: map[string]draw.Point{c[0].id: {}}, routes: map[[2]string][]draw.Point{},
				centers: map[string]float32{c[0].id: c[0].h / 2}}
			continue
		}
		in := map[string]bool{}
		for _, u := range c {
			in[u.id] = true
		}
		var ce []Edge
		for _, e := range edges {
			if in[e.From] {
				ce = append(ce, e)
			}
		}
		parts[i] = flow(c, ce, boardKey, seed, st.Flow)
	}

	// Pack the parts in rows about Aspect times as wide as they are tall.
	var area float64
	var widest float32
	for _, p := range parts {
		area += float64(p.w+st.PackGap) * float64(p.h+st.PackGap)
		widest = max(widest, p.w)
	}
	rowW := max(widest, float32(math.Sqrt(area*float64(st.Aspect))))
	var x, y, rowH, right, bottom float32
	for i, p := range parts {
		if x > 0 && x+p.w > rowW {
			y += rowH + st.PackGap
			x, rowH = 0, 0
		}
		p.shift(st.Margin+x, st.Margin+y)
		x += p.w + st.PackGap
		rowH = max(rowH, p.h)
		right = max(right, st.Margin+x-st.PackGap)
		bottom = max(bottom, st.Margin+y+p.h)
		for _, u := range comps[i] {
			at := p.pos[u.id]
			pl.Centers[u.id] = at.Y + u.h/2
			b, isGroup := blocks[u.id]
			if !isGroup {
				pl.Boxes[u.id] = Box{at.X, at.Y, u.w, u.h}
				continue
			}
			pl.Groups[u.id] = Box{at.X, at.Y, b.w, b.h}
			b.shift(at.X, at.Y)
			for id, q := range b.pos {
				c := cards[id]
				pl.Boxes[id] = Box{q.X, q.Y, c.w, c.total}
			}
			for id, cy := range b.centers {
				pl.Centers[id] = cy
			}
		}
		for id, cy := range p.centers {
			if _, ok := pl.Centers[id]; !ok {
				pl.Centers[id] = cy
			}
		}
	}

	// Arrows, in link order: a flow's own route where the flow has one,
	// else a curve from card to card.
	for _, l := range links {
		a, okA := pl.Boxes[l.From]
		b, okB := pl.Boxes[l.To]
		if !okA || !okB {
			continue
		}
		ga, gb := groupOf(l.From), groupOf(l.To)
		var pts []draw.Point
		switch {
		case ga != "" && ga == gb:
			pts = blocks[ga].routes[[2]string{l.From, l.To}]
		case ga == "" && gb == "":
			pts = parts[slices.IndexFunc(comps, func(c []fnode) bool {
				return slices.ContainsFunc(c, func(u fnode) bool { return u.id == l.From })
			})].routes[[2]string{l.From, l.To}]
		}
		if len(pts) == 0 {
			pts = Curve(draw.Point{X: a.X + a.W, Y: a.Y + cards[l.From].h/2}, draw.Point{X: b.X, Y: b.Y + cards[l.To].h/2})
		}
		r := Arrow{From: l.From, To: l.To, Pts: make([]float32, 0, 2*len(pts))}
		for _, p := range pts {
			r.Pts = append(r.Pts, p.X, p.Y)
		}
		pl.Arrows = append(pl.Arrows, r)
	}

	if notes := doc.Notes(); len(notes) > 0 {
		w, h := sz.Notes(notes)
		nx := st.Margin
		if len(units) > 0 {
			nx = right + st.NotesGap
		}
		pl.Notes = &Box{nx, st.Margin, w, h}
		right = max(right, nx+w)
		bottom = max(bottom, st.Margin+h)
	}
	for _, r := range pl.Arrows {
		for i := 0; i+1 < len(r.Pts); i += 2 {
			right, bottom = max(right, r.Pts[i]), max(bottom, r.Pts[i+1])
		}
	}
	if right > 0 {
		pl.W, pl.H = right+st.Margin, bottom+st.Margin
	}
	pl.round()
	return pl
}

// layoutGroup lays out a group's cards inside its frame.
func layoutGroup(g *board.Group, members []string, links []board.Link, cards map[string]cardSize, cardNode func(string) fnode,
	key func(a, b string) int, seed *Output, sz Sizer, st BoardStyle, groupOf func(string) string) *part {
	labelW := sz.GroupLabel(g.Label) + 2*st.GroupPad
	if len(members) == 0 {
		return &part{w: max(st.EmptyGroupW, labelW), h: st.EmptyGroupH + st.GroupLabelH,
			pos: map[string]draw.Point{}, routes: map[[2]string][]draw.Point{}, centers: map[string]float32{}}
	}
	var edges []Edge
	for _, l := range links {
		if groupOf(l.From) == g.ID && groupOf(l.To) == g.ID {
			edges = append(edges, Edge{From: l.From, To: l.To})
		}
	}
	var p *part
	if len(edges) > 0 {
		nodes := make([]fnode, len(members))
		for i, id := range members {
			nodes[i] = cardNode(id)
		}
		p = flow(nodes, edges, key, seed, st.Flow)
	} else {
		// A list, in the order the cards came up or as sorted, in columns
		// when it's long.
		ids := slices.Clone(members)
		if key != nil {
			slices.SortStableFunc(ids, key)
		}
		p = &part{pos: map[string]draw.Point{}, routes: map[[2]string][]draw.Point{}, centers: map[string]float32{}}
		cols := (len(ids) + st.MaxList - 1) / st.MaxList
		per := (len(ids) + cols - 1) / cols
		var x float32
		for c := 0; c < cols; c++ {
			var y, colW float32
			for _, id := range ids[c*per : min((c+1)*per, len(ids))] {
				cs := cards[id]
				p.pos[id] = draw.Point{X: x, Y: y}
				p.centers[id] = y + cs.total/2
				y += cs.total + st.ListGap
				colW = max(colW, cs.w)
			}
			p.h = max(p.h, y-st.ListGap)
			x += colW + st.ListColGap
		}
		p.w = x - st.ListColGap
	}
	p.shift(st.GroupPad, st.GroupPad+st.GroupLabelH)
	p.w = max(p.w+2*st.GroupPad, labelW)
	p.h += 2*st.GroupPad + st.GroupLabelH
	return p
}

// round snaps every coordinate to 1/16 of a unit: exact in binary, short
// in JSON, and the same after a round trip.
func (pl *Placement) round() {
	r := func(v float32) float32 { return float32(math.Round(float64(v)*16) / 16) }
	rb := func(b Box) Box { return Box{r(b.X), r(b.Y), r(b.W), r(b.H)} }
	pl.W, pl.H = r(pl.W), r(pl.H)
	for id, b := range pl.Boxes {
		pl.Boxes[id] = rb(b)
	}
	for id, b := range pl.Groups {
		pl.Groups[id] = rb(b)
	}
	if pl.Notes != nil {
		n := rb(*pl.Notes)
		pl.Notes = &n
	}
	for _, rt := range pl.Arrows {
		for i := range rt.Pts {
			rt.Pts[i] = r(rt.Pts[i])
		}
	}
	for id, c := range pl.Centers {
		pl.Centers[id] = r(c)
	}
}
