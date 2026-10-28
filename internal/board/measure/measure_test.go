package measure

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
)

func apply(t *testing.T, d *board.Doc, ops ...board.Op) {
	t.Helper()
	for _, op := range ops {
		if err := d.Apply(op); err != nil {
			t.Fatalf("%s %s: %v", op.Name, op.ID, err)
		}
	}
}

// meeting is a board like a meeting makes: a flow, a comparison in two
// groups, loose ideas and notes.
func meeting(t *testing.T) *board.Doc {
	d := board.New()
	apply(t, d,
		board.Op{Name: board.AddNode, ID: "build", Kind: board.Step, Label: "Build"},
		board.Op{Name: board.AddNode, ID: "test", Kind: board.Step, Label: "Test", After: "build"},
		board.Op{Name: board.AddNode, ID: "deploy", Kind: board.Step, Label: "Deploy to production", After: "test"},
		board.Op{Name: board.Frame, ID: "cloud", Label: "Cloud options"},
		board.Op{Name: board.AddNode, ID: "gcp", Kind: board.Option, Label: "Google Cloud", Group: "cloud"},
		board.Op{Name: board.AddNode, ID: "aws", Kind: board.Option, Label: "AWS", Group: "cloud"},
		board.Op{Name: board.AddNode, ID: "azure", Kind: board.Option, Label: "Azure", Group: "cloud"},
		board.Op{Name: board.AddNode, ID: "cheap", Kind: board.Pro, Label: "Cheaper at our size", On: "aws"},
		board.Op{Name: board.AddNode, ID: "idea1", Kind: board.Idea, Label: "Feature flags"},
		board.Op{Name: board.AddNode, ID: "idea2", Kind: board.Question, Label: "Who owns on-call?"},
		board.Op{Name: board.Notes, Lines: []string{"Decided: ship Friday", "Sam: write the runbook by Thursday, with the rollback steps spelled out"}},
	)
	return d
}

func checkNoOverlaps(t *testing.T, d *board.Doc, pl *layout.Placement) {
	t.Helper()
	ids := make([]string, 0, len(pl.Boxes))
	for id := range pl.Boxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if !pl.Boxes[a].Rect().Intersect(pl.Boxes[b].Rect()).Empty() {
				t.Errorf("%s and %s overlap", a, b)
			}
		}
		if pl.Notes != nil && !pl.Boxes[a].Rect().Intersect(pl.Notes.Rect()).Empty() {
			t.Errorf("%s is under the notes", a)
		}
		for g, r := range pl.Groups {
			in := d.Node(a).Group == g
			inside := r.Rect().Intersect(pl.Boxes[a].Rect()) == pl.Boxes[a].Rect()
			touches := !r.Rect().Intersect(pl.Boxes[a].Rect()).Empty()
			if in && !inside {
				t.Errorf("%s sticks out of its frame %s", a, g)
			}
			if !in && touches {
				t.Errorf("frame %s covers %s", g, a)
			}
		}
	}
}

func TestPlaceMeeting(t *testing.T) {
	d := meeting(t)
	pl := Place(d, nil)
	checkNoOverlaps(t, d, pl)
	if len(pl.Arrows) != 2 || pl.Notes == nil || len(pl.Groups) != 1 {
		t.Fatalf("arrows %d, notes %v, groups %d", len(pl.Arrows), pl.Notes, len(pl.Groups))
	}
	for _, b := range pl.Boxes {
		if b.X+b.W > pl.W || b.Y+b.H > pl.H {
			t.Fatalf("a box %+v is outside the board %vx%v", b, pl.W, pl.H)
		}
	}
	// A list keeps the order the cards came up in.
	if !(pl.Boxes["gcp"].Y < pl.Boxes["aws"].Y && pl.Boxes["aws"].Y < pl.Boxes["azure"].Y) {
		t.Fatalf("list order: gcp %v aws %v azure %v", pl.Boxes["gcp"].Y, pl.Boxes["aws"].Y, pl.Boxes["azure"].Y)
	}
	// Sorted, it's alphabetical.
	apply(t, d, board.Op{Name: board.Sort, Group: "cloud", By: board.ByAlpha})
	pl = Place(d, pl)
	if !(pl.Boxes["aws"].Y < pl.Boxes["azure"].Y && pl.Boxes["azure"].Y < pl.Boxes["gcp"].Y) {
		t.Fatalf("alpha order: aws %v azure %v gcp %v", pl.Boxes["aws"].Y, pl.Boxes["azure"].Y, pl.Boxes["gcp"].Y)
	}
	checkNoOverlaps(t, d, pl)
}

// TestPlaceIsTheSameEverywhere: laying out a board twice, with sizers
// that share nothing, through JSON as the wire carries it, gives the same
// placement.
func TestPlaceIsTheSameEverywhere(t *testing.T) {
	d := meeting(t)
	a := Place(d, nil)
	var z Sizer // a fresh cache
	b := layout.Board(d, &z, nil)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("two layouts of one board differ")
	}
	var back layout.Placement
	if err := json.Unmarshal(ja, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&back, a) {
		t.Fatal("the placement changed through JSON")
	}
	// Seeded from itself, nothing moves.
	c := Place(d, &back)
	if !reflect.DeepEqual(c.Boxes, a.Boxes) {
		t.Fatal("a placement seeded from itself moved")
	}
}

// TestManyLooseIdeasPack: a brainstorm of unconnected boxes is packed into
// rows, not one long column.
func TestManyLooseIdeasPack(t *testing.T) {
	d := board.New()
	for i := range 16 {
		apply(t, d, board.Op{Name: board.AddNode, ID: fmt.Sprintf("i%d", i), Kind: board.Idea, Label: fmt.Sprintf("Idea number %d", i)})
	}
	pl := Place(d, nil)
	checkNoOverlaps(t, d, pl)
	if pl.W < pl.H || pl.W > 3*pl.H {
		t.Fatalf("board is %v x %v", pl.W, pl.H)
	}
	apply(t, d, board.Op{Name: board.Sort, By: board.ByCustom, IDs: []string{"i15", "i14"}})
	pl = Place(d, pl)
	if b, c := pl.Boxes["i15"], pl.Boxes["i0"]; !(b.Y < c.Y || b.Y == c.Y && b.X < c.X) {
		t.Fatalf("custom order put i15 at %+v after i0 at %+v", b, c)
	}
}

func TestLongListWraps(t *testing.T) {
	d := board.New()
	apply(t, d, board.Op{Name: board.Frame, ID: "g", Label: "Everything we heard"})
	for i := range 12 {
		apply(t, d, board.Op{Name: board.AddNode, ID: fmt.Sprintf("n%d", i), Kind: board.Fact, Label: fmt.Sprintf("Fact %d", i), Group: "g"})
	}
	pl := Place(d, nil)
	checkNoOverlaps(t, d, pl)
	xs := map[float32]bool{}
	for _, b := range pl.Boxes {
		xs[b.X] = true
	}
	if len(xs) != 2 {
		t.Fatalf("12 cards in %d columns, want 2", len(xs))
	}
}

func TestEmptyBoardAndNotesOnly(t *testing.T) {
	if pl := Place(board.New(), nil); pl.W != 0 || len(pl.Boxes) != 0 {
		t.Fatalf("empty board placed as %+v", pl)
	}
	d := board.New()
	apply(t, d, board.Op{Name: board.Notes, Lines: []string{"Only notes"}}, board.Op{Name: board.Frame, ID: "empty", Label: "Later"})
	pl := Place(d, nil)
	if pl.Notes == nil || pl.Groups["empty"].W == 0 {
		t.Fatalf("notes %v, empty frame %+v", pl.Notes, pl.Groups["empty"])
	}
}
