package fixturemeet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.txt")
	os.WriteFile(p, []byte(`# a meeting
@voice Sam Samantha
@gap 0.5
Sam: Hello.
Sam [command]: Whiteboard, draw it.  # a comment
& Kim [question]: Whiteboard, what's that?
@pause 2
@repeat 3
Kim: Round {n}.
@pause 10
@end
Sam [distractor]: The whiteboard is full.
`), 0o644)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "m" || len(s.Lines) != 7 || s.Voices["Sam"] != "Samantha" || s.Voices["Kim"] == "" || s.Voices["Kim"] == "Samantha" {
		t.Fatalf("script = %+v", s)
	}
	l := s.Lines
	if l[1].Label != Command || l[1].Text != "Whiteboard, draw it." || !l[2].With || l[2].Label != Question {
		t.Fatalf("lines = %+v", l[:3])
	}
	// The pause before the block, then the block's own pause before each
	// later round and before the line after it.
	if l[3].Text != "Round 1." || l[3].Pause != 2.5 || l[4].Text != "Round 2." || l[4].Pause != 10.5 || l[6].Pause != 10.5 {
		t.Fatalf("rounds = %+v", l[3:])
	}
	if !l[1].Addressed() || l[6].Addressed() {
		t.Fatal("Addressed is wrong")
	}
}
