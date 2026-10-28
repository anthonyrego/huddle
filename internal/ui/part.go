package ui

import (
	"fmt"
	"strings"
)

// Parts name addressable pieces inside a widget, for hover, comments and
// selection. The designer's are node:<id> for a card and
// node:<id>/<response> for a row and its route.
type PartKind string

const PartNode PartKind = "node"

// Part is a PartKind plus its id, e.g. {PartNode, "intent/billing"}.
type Part struct {
	Kind PartKind
	ID   string
}

// String formats a part as "<kind>:<id>", or "" for the zero Part.
func (p Part) String() string {
	if p.Kind == "" {
		return ""
	}
	return string(p.Kind) + ":" + p.ID
}

// ParsePart parses "<kind>:<id>". The empty string is the zero Part.
func ParsePart(s string) (Part, error) {
	if s == "" {
		return Part{}, nil
	}
	kind, id, ok := strings.Cut(s, ":")
	if !ok || id == "" {
		return Part{}, fmt.Errorf("part %q: want <kind>:<id>", s)
	}
	if k := PartKind(kind); k == PartNode {
		return Part{Kind: k, ID: id}, nil
	}
	return Part{}, fmt.Errorf("part %q: unknown kind %q", s, kind)
}
