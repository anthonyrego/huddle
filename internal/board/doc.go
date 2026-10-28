// Package board is what's on the whiteboard: boxes, arrows between them,
// frames around some, and the short notes under them. The model changes it
// with ops (its tool calls); the board is always the replay of the ops it
// has accepted, so dropping an op is replaying without it.
//
// Copied from an earlier project of the author's.
package board

import (
	"slices"
	"strings"
)

// Kind is what a box stands for.
type Kind string

const (
	Idea     Kind = "idea"     // a thing, concept or point
	Step     Kind = "step"     // one step in an order
	Option   Kind = "option"   // one of the choices being weighed
	Person   Kind = "person"   // someone involved
	Fact     Kind = "fact"     // a number, date, name or definition
	Question Kind = "question" // something still open
	Example  Kind = "example"  // a concrete example
	Pro      Kind = "pro"      // an upside of the box it's on
	Con      Kind = "con"      // a downside or risk of the box it's on
)

// The kinds boards made for mapping someone's work used.
const (
	Decision      Kind = "decision"       // a choice a person makes
	Tool          Kind = "tool"           // a program or system
	Data          Kind = "data"           // where work comes from or goes
	Gap           Kind = "gap"            // something still to ask about
	Pain          Kind = "pain"           // what's slow or annoying about a step
	AIOpportunity Kind = "ai_opportunity" // where AI could help with a step
)

// Kinds lists the kinds the model is offered, in the order the tool
// declaration lists them.
var Kinds = []Kind{Idea, Step, Option, Person, Fact, Question, Example, Pro, Con}

// Retired lists kinds no longer offered. Saved boards that use them still
// open and draw as they did.
var Retired = []Kind{Decision, Tool, Data, Gap, Pain, AIOpportunity}

// Valid reports whether k is one of Kinds or Retired.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) || slices.Contains(Retired, k) }

// NeedsHost reports whether a box of this kind must hang from another box:
// a pro or a con (or a pain or an opportunity) is always about another box.
func (k Kind) NeedsHost() bool { return k == Pro || k == Con || k == Pain || k == AIOpportunity }

// Node is a box. A node with On set is a satellite: it's drawn under the
// box it names instead of being laid out on its own.
type Node struct {
	ID     string
	Kind   Kind
	Label  string
	Detail string // one short line under the label
	Note   string // set by the note op
	On     string // the host box, for satellites
	Group  string // the frame it sits in, if any
}

// Link is an arrow.
type Link struct {
	From, To string
	Label    string
}

// ID names a link the way remove takes it.
func (l Link) ID() string { return l.From + "->" + l.To }

// Group is a labeled frame. Its members are the nodes whose Group names it.
type Group struct {
	ID, Label string
}

// Ways to sort a group's members or the board's columns.
const (
	ByAlpha  = "alpha"  // by label
	ByChrono = "chrono" // in the order they were put on the board
	ByCustom = "custom" // in the order given, the rest after in the order they came
)

// Order is how a group's members, or the whole board, are sorted.
type Order struct {
	By  string
	IDs []string // ByCustom: the order wanted
}

// MaxNotes is how many lines the meeting's notes panel keeps.
const MaxNotes = 60

// Doc is the board. Nodes and groups keep the order they were added in,
// which the layout uses to break ties.
type Doc struct {
	nodes  map[string]*Node
	order  []string
	links  []Link
	groups map[string]*Group
	gorder []string
	orders map[string]Order // by group id; "" is the whole board
	notes  []string         // the meeting's notes panel
}

// New returns an empty board.
func New() *Doc {
	return &Doc{nodes: map[string]*Node{}, groups: map[string]*Group{}, orders: map[string]Order{}}
}

// Notes are the lines of the meeting's notes panel.
func (d *Doc) Notes() []string { return slices.Clone(d.notes) }

// OrderOf is how target (a group id, or "" for the whole board) is
// sorted; ok is false when it isn't.
func (d *Doc) OrderOf(target string) (Order, bool) {
	o, ok := d.orders[target]
	return o, ok
}

// Compare returns a comparison of node or group ids for target's order,
// or nil when target isn't sorted. A group stands for its first member.
func (d *Doc) Compare(target string) func(a, b string) int {
	o, ok := d.orders[target]
	if !ok {
		return nil
	}
	index := func(id string) int {
		if g := d.groups[id]; g != nil {
			best := len(d.order) + slices.Index(d.gorder, id)
			for i, nid := range d.order {
				if d.nodes[nid].Group == id {
					return i
				}
			}
			return best
		}
		return slices.Index(d.order, id)
	}
	label := func(id string) string {
		if n := d.nodes[id]; n != nil {
			return strings.ToLower(n.Label)
		}
		if g := d.groups[id]; g != nil {
			return strings.ToLower(g.Label)
		}
		return id
	}
	return func(a, b string) int {
		switch o.By {
		case ByAlpha:
			if c := strings.Compare(label(a), label(b)); c != 0 {
				return c
			}
		case ByCustom:
			ia, ib := slices.Index(o.IDs, a), slices.Index(o.IDs, b)
			if ia < 0 {
				ia = len(o.IDs)
			}
			if ib < 0 {
				ib = len(o.IDs)
			}
			if ia != ib {
				return ia - ib
			}
		}
		return index(a) - index(b)
	}
}

// Node returns the node with id, or nil.
func (d *Doc) Node(id string) *Node { return d.nodes[id] }

// Nodes returns every node in the order they were added.
func (d *Doc) Nodes() []*Node {
	out := make([]*Node, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, d.nodes[id])
	}
	return out
}

// Links returns the arrows in the order they were drawn.
func (d *Doc) Links() []Link { return slices.Clone(d.links) }

// Groups returns the frames in the order they were drawn.
func (d *Doc) Groups() []*Group {
	out := make([]*Group, 0, len(d.gorder))
	for _, id := range d.gorder {
		out = append(out, d.groups[id])
	}
	return out
}

// Group returns the frame with id, or nil.
func (d *Doc) Group(id string) *Group { return d.groups[id] }

// Members returns the nodes in a frame, in the order they were added.
func (d *Doc) Members(group string) []*Node {
	var out []*Node
	for _, n := range d.Nodes() {
		if n.Group == group {
			out = append(out, n)
		}
	}
	return out
}

// Satellites returns the nodes hanging from host, in the order they were
// added.
func (d *Doc) Satellites(host string) []*Node {
	var out []*Node
	for _, n := range d.Nodes() {
		if n.On == host {
			out = append(out, n)
		}
	}
	return out
}

// Empty reports whether nothing is on the board.
func (d *Doc) Empty() bool { return len(d.order) == 0 && len(d.gorder) == 0 && len(d.notes) == 0 }

// ids lists the node ids, sorted, for error messages.
func (d *Doc) ids() string {
	ids := slices.Clone(d.order)
	slices.Sort(ids)
	if len(ids) == 0 {
		return "(the board is empty)"
	}
	return strings.Join(ids, ", ")
}

func (d *Doc) link(from, to string) int {
	return slices.IndexFunc(d.links, func(l Link) bool { return l.From == from && l.To == to })
}

func (d *Doc) addNode(n *Node) {
	if d.nodes[n.ID] == nil {
		d.order = append(d.order, n.ID)
	}
	d.nodes[n.ID] = n
}

func (d *Doc) removeNode(id string) {
	if d.nodes[id] == nil {
		return
	}
	for _, s := range d.Satellites(id) {
		d.removeNode(s.ID)
	}
	delete(d.nodes, id)
	d.order = slices.DeleteFunc(d.order, func(x string) bool { return x == id })
	d.links = slices.DeleteFunc(d.links, func(l Link) bool { return l.From == id || l.To == id })
}

func (d *Doc) removeGroup(id string) {
	if d.groups[id] == nil {
		return
	}
	for _, n := range d.Members(id) {
		n.Group = ""
	}
	delete(d.groups, id)
	delete(d.orders, id)
	d.gorder = slices.DeleteFunc(d.gorder, func(x string) bool { return x == id })
}

// Ops are ops that rebuild the board from empty: frames, then boxes in the
// order they came (the boxes that hang from others after them), arrows,
// notes under boxes, the notes panel and the orders. A version stores
// these rather than every op since the start, so a long meeting's
// snapshot stays the size of its board.
func (d *Doc) Ops() []Op {
	var ops []Op
	for _, g := range d.Groups() {
		ops = append(ops, Op{Name: Frame, ID: g.ID, Label: g.Label})
	}
	add := func(n *Node) {
		ops = append(ops, Op{Name: AddNode, ID: n.ID, Kind: n.Kind, Label: n.Label, Detail: n.Detail, On: n.On, Group: n.Group})
	}
	for _, n := range d.Nodes() {
		if n.On == "" {
			add(n)
		}
	}
	for _, n := range d.Nodes() {
		if n.On != "" {
			add(n)
		}
	}
	for _, l := range d.links {
		ops = append(ops, Op{Name: Connect, From: l.From, To: l.To, Label: l.Label})
	}
	for _, n := range d.Nodes() {
		if n.Note != "" {
			ops = append(ops, Op{Name: Annotate, Target: n.ID, Text: n.Note})
		}
	}
	if len(d.notes) > 0 {
		ops = append(ops, Op{Name: Notes, Lines: slices.Clone(d.notes), Replace: true})
	}
	targets := make([]string, 0, len(d.orders))
	for t := range d.orders {
		targets = append(targets, t)
	}
	slices.Sort(targets)
	for _, t := range targets {
		o := d.orders[t]
		ops = append(ops, Op{Name: Sort, Group: t, By: o.By, IDs: slices.Clone(o.IDs)})
	}
	return ops
}
