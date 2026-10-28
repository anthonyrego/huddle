package board

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Snapshot is a board as one version stores it: the ops that rebuild it
// from empty, in order. It is the Doc of wire.BoardVersion. Replaying the
// same ops gives the same board, so the server and every app agree on what
// a version holds; where its boxes go is the server's layout, sent beside it.
type Snapshot struct {
	Ops []Op `json:"ops"`
}

// Doc replays the snapshot. Ops that don't apply are reported, not fatal:
// a snapshot written by an older server may hold ops this one checks
// differently.
func (s Snapshot) Doc() (*Doc, []error) { return Replay(s.Ops) }

// MarshalSnapshot encodes the ops as a snapshot.
func MarshalSnapshot(ops []Op) (json.RawMessage, error) {
	return json.Marshal(Snapshot{Ops: ops})
}

// UnmarshalSnapshot decodes a snapshot, normalizing each op as a fixture
// line is.
func UnmarshalSnapshot(data []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("board: snapshot: %w", err)
	}
	for i := range s.Ops {
		s.Ops[i].normalize()
	}
	return s, nil
}

// ReadOps reads a fixture: one JSON op per line, blank lines and # comments
// skipped.
func ReadOps(path string) ([]Op, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ops []Op
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		op, err := ParseOp([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		ops = append(ops, op)
	}
	return ops, sc.Err()
}
