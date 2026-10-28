package board

import (
	"fmt"
	"strconv"
	"strings"
)

// Digest writes the board as text for the model: what get_board returns
// and what a fresh session is seeded with. Satellites are indented under
// their host.
func (d *Doc) Digest() string {
	var b strings.Builder
	if d.Empty() {
		return "The board is empty."
	}
	fmt.Fprintf(&b, "BOARD: %d boxes, %d arrows, %d groups\n", len(d.order), len(d.links), len(d.gorder))
	for _, g := range d.Groups() {
		fmt.Fprintf(&b, "group %s %s\n", g.ID, strconv.Quote(g.Label))
	}
	for _, n := range d.Nodes() {
		if n.On != "" {
			continue
		}
		b.WriteString(nodeLine(n, ""))
		for _, s := range d.Satellites(n.ID) {
			b.WriteString(nodeLine(s, "  "))
		}
	}
	for _, l := range d.links {
		fmt.Fprintf(&b, "arrow %s", l.ID())
		if l.Label != "" {
			fmt.Fprintf(&b, " %s", strconv.Quote(l.Label))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func nodeLine(n *Node, indent string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s %s %s", indent, n.Kind, n.ID, strconv.Quote(n.Label))
	if n.Group != "" {
		fmt.Fprintf(&b, " group=%s", n.Group)
	}
	if n.Detail != "" {
		fmt.Fprintf(&b, " detail=%s", strconv.Quote(n.Detail))
	}
	if n.Note != "" {
		fmt.Fprintf(&b, " note=%s", strconv.Quote(n.Note))
	}
	b.WriteByte('\n')
	return b.String()
}
