package board

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Op names, which are also the tool names the model calls.
const (
	AddNode  = "add_node"
	Connect  = "connect"
	Frame    = "group"
	Annotate = "note"
	Rename   = "rename"
	Remove   = "remove"
	PointAt  = "point_at"
	GetBoard = "get_board"
	Sort     = "order"
	Notes    = "notes"

	// What the server does itself rather than the board: a new version
	// from an older one, the spoken-answer flag, a read of the stored
	// transcript and auto mode.
	Undo       = "undo"
	RevertTo   = "revert_to"
	Speak      = "speak"
	Transcript = "transcript"
	Auto       = "auto"
)

// DefaultPointMS is how long point_at highlights when it doesn't say.
const DefaultPointMS = 1800

// Op is one change to the board: a tool call's arguments, or a line of a
// fixture. Field names are the tools' parameter names.
type Op struct {
	Name    string   `json:"op"`
	ID      string   `json:"id,omitempty"`
	Kind    Kind     `json:"kind,omitempty"`
	Label   string   `json:"label,omitempty"`
	After   string   `json:"after,omitempty"`
	On      string   `json:"on,omitempty"`
	Group   string   `json:"group,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Members []string `json:"members,omitempty"`
	IDs     []string `json:"ids,omitempty"`
	Target  string   `json:"target,omitempty"`
	Text    string   `json:"text,omitempty"`
	MS      int      `json:"ms,omitempty"`
	By      string   `json:"by,omitempty"`      // order
	Lines   []string `json:"lines,omitempty"`   // notes
	Replace bool     `json:"replace,omitempty"` // notes
	Version int64    `json:"version,omitempty"` // revert_to
	FromMs  int64    `json:"from_ms,omitempty"` // transcript
	ToMs    int64    `json:"to_ms,omitempty"`   // transcript
	Enabled bool     `json:"enabled,omitempty"` // auto
}

// Changes reports whether the op changes the board itself, so it belongs
// in a version's snapshot. Undo and revert_to make versions too, but out
// of older snapshots (Versioned), not by changing this one.
func (o Op) Changes() bool {
	switch o.Name {
	case PointAt, GetBoard, Undo, RevertTo, Speak, Transcript, Auto:
		return false
	}
	return true
}

// Versioned reports whether the op makes a version from an older one.
func (o Op) Versioned() bool { return o.Name == Undo || o.Name == RevertTo }

// Decode turns a tool call into an op. It forgives what a voice model gets
// loosely right: a lone id where a list is expected, ids with spaces or
// capitals.
func Decode(name string, args map[string]any) (Op, error) {
	fixed := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok && (k == "ids" || k == "members") {
			v = strings.Split(s, ",")
		}
		if s, ok := v.(string); ok && k == "lines" {
			v = []string{s}
		}
		fixed[k] = v
	}
	data, err := json.Marshal(fixed)
	if err != nil {
		return Op{}, err
	}
	var op Op
	if err := json.Unmarshal(data, &op); err != nil {
		return Op{}, fmt.Errorf("%s: %w", name, err)
	}
	op.Name = name
	op.normalize()
	return op, nil
}

// ParseOp reads one fixture line.
func ParseOp(line []byte) (Op, error) {
	var op Op
	if err := json.Unmarshal(line, &op); err != nil {
		return Op{}, err
	}
	op.normalize()
	return op, nil
}

func (o *Op) normalize() {
	for _, p := range []*string{&o.ID, &o.After, &o.On, &o.Group, &o.From, &o.To, &o.Target} {
		*p = NormID(*p)
	}
	for _, ids := range []*[]string{&o.Members, &o.IDs} {
		out := (*ids)[:0]
		for _, id := range *ids {
			if id = NormID(id); id != "" {
				out = append(out, id)
			}
		}
		*ids = out
	}
	o.Kind = Kind(strings.ToLower(strings.TrimSpace(string(o.Kind))))
	o.By = normBy(o.By)
	if o.Group == "board" {
		o.Group = ""
	}
	lines := o.Lines[:0]
	for _, l := range o.Lines {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-•*")); l != "" {
			lines = append(lines, l)
		}
	}
	o.Lines = lines
	o.Label = strings.TrimSpace(o.Label)
	o.Detail = strings.TrimSpace(o.Detail)
	o.Text = strings.TrimSpace(o.Text)
}

// normBy reads the ways a model names a sort.
func normBy(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "alpha"), s == "name", s == "label", s == "a-z":
		return ByAlpha
	case strings.HasPrefix(s, "chrono"), s == "time", s == "added", s == "order added":
		return ByChrono
	case s == "custom", s == "given", s == "manual":
		return ByCustom
	}
	return s
}

// NormID lowercases an id and joins its words with underscores. A link id
// keeps its arrow ("a->b").
func NormID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if from, to, ok := strings.Cut(s, "->"); ok {
		return NormID(from) + "->" + NormID(to)
	}
	return strings.Join(strings.Fields(s), "_")
}

// Apply checks op against the board and, only if it's valid, applies it.
// Errors are written for the model: they say what's wrong and what is on
// the board.
func (d *Doc) Apply(op Op) error {
	switch op.Name {
	case AddNode:
		return d.addOp(op)
	case Connect:
		return d.connectOp(op)
	case Frame:
		return d.groupOp(op)
	case Annotate:
		n := d.nodes[op.Target]
		if n == nil {
			return d.noBox(op.Target)
		}
		n.Note = op.Text
		return nil
	case Rename:
		if op.Label == "" {
			return fmt.Errorf("rename %s: label is empty", op.ID)
		}
		if n := d.nodes[op.ID]; n != nil {
			n.Label = op.Label
			return nil
		}
		if g := d.groups[op.ID]; g != nil {
			g.Label = op.Label
			return nil
		}
		return d.noBox(op.ID)
	case Remove:
		return d.removeOp(op)
	case PointAt:
		if len(op.IDs) == 0 {
			return fmt.Errorf("point_at: ids is empty")
		}
		for _, id := range op.IDs {
			if d.nodes[id] == nil && d.groups[id] == nil {
				return d.noBox(id)
			}
		}
		return nil
	case GetBoard, Speak, Transcript, Auto:
		return nil
	case Sort:
		return d.sortOp(op)
	case Notes:
		if op.Replace {
			d.notes = nil
		} else if len(op.Lines) == 0 {
			return fmt.Errorf("notes: lines is empty")
		}
		d.notes = append(d.notes, op.Lines...)
		if len(d.notes) > MaxNotes {
			d.notes = d.notes[len(d.notes)-MaxNotes:]
		}
		return nil
	case Undo, RevertTo:
		return fmt.Errorf("%s makes a version from an older one; only the server applies it", op.Name)
	}
	return fmt.Errorf("unknown op %q", op.Name)
}

func (d *Doc) sortOp(op Op) error {
	target := op.Group
	if target != "" && d.groups[target] == nil {
		return fmt.Errorf("order: no group %q (leave group out to sort the whole board)", target)
	}
	switch op.By {
	case "none":
		delete(d.orders, target)
		return nil
	case ByAlpha, ByChrono:
		d.orders[target] = Order{By: op.By}
		return nil
	case ByCustom:
		if len(op.IDs) == 0 {
			return fmt.Errorf("order: a custom order needs ids, in the order wanted")
		}
		for _, id := range op.IDs {
			if d.nodes[id] == nil && d.groups[id] == nil {
				return d.noBox(id)
			}
		}
		d.orders[target] = Order{By: ByCustom, IDs: slices.Clone(op.IDs)}
		return nil
	}
	return fmt.Errorf("order: by %q isn't alpha, chrono, custom or none", op.By)
}

func (d *Doc) noBox(id string) error {
	if id == "" {
		return fmt.Errorf("no id given (boxes: %s)", d.ids())
	}
	return fmt.Errorf("no box %q on the board (boxes: %s)", id, d.ids())
}

func (d *Doc) addOp(op Op) error {
	if op.ID == "" {
		return fmt.Errorf("add_node: id is empty")
	}
	if strings.Contains(op.ID, "->") {
		return fmt.Errorf("add_node %s: ids can't contain ->", op.ID)
	}
	old := d.nodes[op.ID]
	if d.groups[op.ID] != nil {
		return fmt.Errorf("add_node %s: that id is a group's", op.ID)
	}
	n := &Node{ID: op.ID, Kind: Idea}
	if old != nil {
		cp := *old
		n = &cp
	}
	if op.Kind != "" {
		if !op.Kind.Valid() {
			return fmt.Errorf("add_node %s: kind %q isn't one of %v", op.ID, op.Kind, Kinds)
		}
		n.Kind = op.Kind
	}
	if op.Label != "" {
		n.Label = op.Label
	}
	if n.Label == "" {
		return fmt.Errorf("add_node %s: label is empty", op.ID)
	}
	if op.Detail != "" {
		n.Detail = op.Detail
	}
	if op.On != "" {
		host := d.nodes[op.On]
		switch {
		case host == nil:
			return d.noBox(op.On)
		case op.On == op.ID:
			return fmt.Errorf("add_node %s: a box can't hang from itself", op.ID)
		case host.On != "":
			return fmt.Errorf("add_node %s: %s already hangs under %s; put it on %s", op.ID, op.On, host.On, host.On)
		}
		n.On = op.On
	}
	if n.Kind.NeedsHost() && n.On == "" {
		return fmt.Errorf("add_node %s: a %s needs on, the id of the box it's about", op.ID, n.Kind)
	}
	if op.Group != "" {
		if d.groups[op.Group] == nil {
			return fmt.Errorf("add_node %s: no group %q (make it with group first)", op.ID, op.Group)
		}
		n.Group = op.Group
	}
	if op.After != "" {
		switch {
		case d.nodes[op.After] == nil:
			return d.noBox(op.After)
		case op.After == op.ID:
			return fmt.Errorf("add_node %s: a box can't follow itself", op.ID)
		case n.On != "":
			return fmt.Errorf("add_node %s: it hangs under %s, so it can't follow another box", op.ID, n.On)
		case d.nodes[op.After].On != "":
			return fmt.Errorf("add_node %s: %s hangs under %s; follow %s instead", op.ID, op.After, d.nodes[op.After].On, d.nodes[op.After].On)
		}
	}

	d.addNode(n)
	if n.On != "" {
		// A satellite has no arrows of its own, and none hang from it.
		d.links = deleteLinks(d.links, n.ID)
		for _, s := range d.Satellites(n.ID) {
			s.On = n.On
		}
	}
	if op.After != "" && d.link(op.After, n.ID) < 0 {
		d.links = append(d.links, Link{From: op.After, To: n.ID})
	}
	return nil
}

func deleteLinks(ls []Link, id string) []Link {
	out := ls[:0]
	for _, l := range ls {
		if l.From != id && l.To != id {
			out = append(out, l)
		}
	}
	return out
}

func (d *Doc) connectOp(op Op) error {
	for _, id := range []string{op.From, op.To} {
		n := d.nodes[id]
		if n == nil {
			return d.noBox(id)
		}
		if n.On != "" {
			return fmt.Errorf("connect: %s hangs under %s; arrows join the boxes it hangs from", id, n.On)
		}
	}
	if op.From == op.To {
		return fmt.Errorf("connect: from and to are both %s", op.From)
	}
	if i := d.link(op.From, op.To); i >= 0 {
		d.links[i].Label = op.Label
		return nil
	}
	d.links = append(d.links, Link{From: op.From, To: op.To, Label: op.Label})
	return nil
}

func (d *Doc) groupOp(op Op) error {
	if op.ID == "" {
		return fmt.Errorf("group: id is empty")
	}
	if d.nodes[op.ID] != nil {
		return fmt.Errorf("group %s: that id is a box's", op.ID)
	}
	g := d.groups[op.ID]
	if g == nil && op.Label == "" {
		return fmt.Errorf("group %s: label is empty", op.ID)
	}
	for _, id := range op.Members {
		n := d.nodes[id]
		if n == nil {
			return d.noBox(id)
		}
		if n.On != "" {
			return fmt.Errorf("group %s: %s hangs under %s, so it goes where %s goes", op.ID, id, n.On, n.On)
		}
	}
	if g == nil {
		g = &Group{ID: op.ID}
		d.groups[op.ID] = g
		d.gorder = append(d.gorder, op.ID)
	}
	if op.Label != "" {
		g.Label = op.Label
	}
	for _, id := range op.Members {
		d.nodes[id].Group = op.ID
	}
	return nil
}

func (d *Doc) removeOp(op Op) error {
	if len(op.IDs) == 0 {
		return fmt.Errorf("remove: ids is empty")
	}
	for _, id := range op.IDs {
		if from, to, ok := strings.Cut(id, "->"); ok {
			if d.link(from, to) < 0 {
				return fmt.Errorf("remove: no arrow %s", id)
			}
			continue
		}
		if d.nodes[id] == nil && d.groups[id] == nil {
			return d.noBox(id)
		}
	}
	for _, id := range op.IDs {
		if from, to, ok := strings.Cut(id, "->"); ok {
			if i := d.link(from, to); i >= 0 {
				d.links = append(d.links[:i], d.links[i+1:]...)
			}
			continue
		}
		d.removeNode(id)
		d.removeGroup(id)
	}
	return nil
}

// Replay builds a board from ops in order, skipping (and returning) the
// ones that don't apply.
func Replay(ops []Op) (*Doc, []error) {
	d := New()
	var errs []error
	for i, op := range ops {
		if err := d.Apply(op); err != nil {
			errs = append(errs, fmt.Errorf("op %d: %w", i+1, err))
		}
	}
	return d, errs
}
