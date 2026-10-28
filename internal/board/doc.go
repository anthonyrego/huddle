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

// Doc is the board. Nodes and groups keep the order they were added in,
// which the layout uses to break ties.
type Doc struct {
	nodes  map[string]*Node
	order  []string
	links  []Link
	groups map[string]*Group
	gorder []string
}

// New returns an empty board.
func New() *Doc {
	return &Doc{nodes: map[string]*Node{}, groups: map[string]*Group{}}
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
func (d *Doc) Empty() bool { return len(d.order) == 0 && len(d.gorder) == 0 }

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
	d.gorder = slices.DeleteFunc(d.gorder, func(x string) bool { return x == id })
}
