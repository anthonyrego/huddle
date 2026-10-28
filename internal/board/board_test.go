package board

import (
	"slices"
	"strings"
	"testing"
)

func mustApply(t *testing.T, d *Doc, ops ...Op) {
	t.Helper()
	for _, op := range ops {
		if err := d.Apply(op); err != nil {
			t.Fatalf("%s %s: %v", op.Name, op.ID, err)
		}
	}
}

func TestAddChainAndSatellites(t *testing.T) {
	d := New()
	mustApply(t, d,
		Op{Name: AddNode, ID: "email_in", Kind: Data, Label: "Order emails"},
		Op{Name: AddNode, ID: "copy", Kind: Step, Label: "Copy into sheet", After: "email_in"},
		Op{Name: AddNode, ID: "slow", Kind: Pain, Label: "Retyping", On: "copy"},
		Op{Name: AddNode, ID: "excel", Kind: Tool, Label: "Excel", On: "copy"},
	)
	if got := d.Links(); len(got) != 1 || got[0].ID() != "email_in->copy" {
		t.Fatalf("links = %+v", got)
	}
	if sats := d.Satellites("copy"); len(sats) != 2 || sats[0].ID != "slow" {
		t.Fatalf("satellites = %+v", sats)
	}
	// Re-adding an id updates the box and keeps what the op leaves out.
	mustApply(t, d, Op{Name: AddNode, ID: "copy", Label: "Copy rows"})
	if n := d.Node("copy"); n.Label != "Copy rows" || n.Kind != Step {
		t.Fatalf("updated node = %+v", n)
	}
	if len(d.Nodes()) != 4 {
		t.Fatalf("update added a node: %d", len(d.Nodes()))
	}
	// Removing a step takes what hangs under it and its arrows.
	mustApply(t, d, Op{Name: Remove, IDs: []string{"copy"}})
	if len(d.Nodes()) != 1 || len(d.Links()) != 0 {
		t.Fatalf("after remove: nodes %d links %d", len(d.Nodes()), len(d.Links()))
	}
}

func TestErrorsLeaveTheBoardAlone(t *testing.T) {
	d := New()
	mustApply(t, d, Op{Name: AddNode, ID: "a", Label: "A"})
	cases := []struct {
		op   Op
		want string
	}{
		{Op{Name: AddNode, ID: "p", Kind: Con, Label: "Slow"}, "needs on"},
		{Op{Name: AddNode, ID: "b", Label: "B", After: "zzz"}, `no box "zzz" on the board (boxes: a)`},
		{Op{Name: AddNode, ID: "b", Kind: "widget", Label: "B"}, "isn't one of"},
		{Op{Name: AddNode, ID: "b", Label: "B", Group: "g"}, "make it with group first"},
		{Op{Name: Connect, From: "a", To: "a"}, "both a"},
		{Op{Name: Frame, ID: "g", Label: "G", Members: []string{"a", "nope"}}, `no box "nope"`},
		{Op{Name: Remove, IDs: []string{"a", "x->y"}}, "no arrow x->y"},
		{Op{Name: PointAt, IDs: []string{"ghost"}}, `no box "ghost"`},
	}
	for _, c := range cases {
		err := d.Apply(c.op)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.op.Name, err, c.want)
		}
	}
	if len(d.Nodes()) != 1 || len(d.Groups()) != 0 || d.Node("a") == nil {
		t.Fatalf("a failed op changed the board:\n%s", d.Digest())
	}
}

func TestSatellitesCantBeJoined(t *testing.T) {
	d := New()
	mustApply(t, d,
		Op{Name: AddNode, ID: "a", Label: "A"},
		Op{Name: AddNode, ID: "b", Label: "B"},
		Op{Name: AddNode, ID: "ai", Kind: AIOpportunity, Label: "AI drafts it", On: "a"},
	)
	if err := d.Apply(Op{Name: Connect, From: "ai", To: "b"}); err == nil {
		t.Fatal("connected a satellite")
	}
	if err := d.Apply(Op{Name: AddNode, ID: "c", Label: "C", On: "ai"}); err == nil || !strings.Contains(err.Error(), "put it on a") {
		t.Fatalf("hung from a satellite: %v", err)
	}
	// Turning a box into a satellite drops its arrows.
	mustApply(t, d, Op{Name: Connect, From: "a", To: "b"}, Op{Name: AddNode, ID: "b", On: "a"})
	if len(d.Links()) != 0 {
		t.Fatalf("satellite kept arrows: %+v", d.Links())
	}
}

func TestKinds(t *testing.T) {
	d := New()
	mustApply(t, d,
		Op{Name: AddNode, ID: "train", Kind: Option, Label: "Take the train"},
		Op{Name: AddNode, ID: "easy", Kind: Pro, Label: "Nap on the way", On: "train"},
		Op{Name: AddNode, ID: "fixed", Kind: Con, Label: "Fixed times", On: "train"},
		Op{Name: AddNode, ID: "cascades", Kind: Example, Label: "Cascades, 8:20", On: "train"},
		Op{Name: AddNode, ID: "salad", Kind: Example, Label: "A salad bar"},
	)
	if sats := d.Satellites("train"); len(sats) != 3 {
		t.Fatalf("satellites = %+v", sats)
	}
	for _, k := range []Kind{Pro, Con} {
		if err := d.Apply(Op{Name: AddNode, ID: "x", Kind: k, Label: "X"}); err == nil || !strings.Contains(err.Error(), "needs on") {
			t.Errorf("%s without on: err = %v", k, err)
		}
	}
	// Saved boards from before keep opening: retired kinds still apply.
	mustApply(t, d,
		Op{Name: AddNode, ID: "copy", Kind: Step, Label: "Copy rows"},
		Op{Name: AddNode, ID: "slow", Kind: Pain, Label: "Retyping", On: "copy"},
		Op{Name: AddNode, ID: "how_often", Kind: Gap, Label: "How often?", After: "copy"},
	)
	if err := d.Apply(Op{Name: AddNode, ID: "y", Kind: AIOpportunity, Label: "Y"}); err == nil {
		t.Error("an ai_opportunity applied without on")
	}
}

func TestDecodeForgivesLooseArgs(t *testing.T) {
	op, err := Decode(PointAt, map[string]any{"ids": "Email In, copy", "ms": 900.0})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.IDs) != 2 || op.IDs[0] != "email_in" || op.IDs[1] != "copy" || op.MS != 900 {
		t.Fatalf("op = %+v", op)
	}
	op, err = Decode(Remove, map[string]any{"ids": []any{"Email In -> Copy"}})
	if err != nil || op.IDs[0] != "email_in->copy" {
		t.Fatalf("op = %+v, err %v", op, err)
	}
}

func TestReplayAndDigest(t *testing.T) {
	ops := []Op{
		{Name: Frame, ID: "intake", Label: "Intake"},
		{Name: AddNode, ID: "email_in", Kind: Data, Label: "Order emails", Group: "intake"},
		{Name: AddNode, ID: "copy", Label: "Copy rows", After: "email_in", Detail: "every Monday"},
		{Name: AddNode, ID: "ai", Kind: AIOpportunity, Label: "AI fills the rows", On: "copy"},
		{Name: AddNode, ID: "bad", After: "missing", Label: "x"},
		{Name: Annotate, Target: "copy", Text: "about 2 hours"},
	}
	d, errs := Replay(ops)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "op 5") {
		t.Fatalf("errs = %v", errs)
	}
	want := `BOARD: 3 boxes, 1 arrows, 1 groups
group intake "Intake"
data email_in "Order emails" group=intake
idea copy "Copy rows" detail="every Monday" note="about 2 hours"
  ai_opportunity ai "AI fills the rows"
arrow email_in->copy`
	if got := d.Digest(); got != want {
		t.Fatalf("digest:\n%s\nwant:\n%s", got, want)
	}
	if New().Digest() != "The board is empty." {
		t.Fatal("empty digest")
	}
}

func TestToolsDeclareEveryOp(t *testing.T) {
	names := map[string]bool{}
	for _, f := range Tools()[0].FunctionDeclarations {
		names[f.Name] = true
		if f.Description == "" {
			t.Errorf("%s has no description", f.Name)
		}
	}
	for _, n := range []string{AddNode, Connect, Frame, Annotate, Rename, Remove, PointAt, GetBoard, Sort, Notes, Undo, RevertTo, Speak, Transcript, Auto} {
		if !names[n] {
			t.Errorf("no tool for %s", n)
		}
	}
}

// TestToolsOfferOnlyCurrentKinds: the model is offered Kinds and never a
// retired kind.
func TestToolsOfferOnlyCurrentKinds(t *testing.T) {
	add := Tools()[0].FunctionDeclarations[0]
	if add.Name != AddNode {
		t.Fatalf("first tool is %s", add.Name)
	}
	var want []string
	for _, k := range Kinds {
		want = append(want, string(k))
	}
	if got := add.Parameters.Properties["kind"].Enum; !slices.Equal(got, want) {
		t.Fatalf("kind enum = %v, want %v", got, want)
	}
	for _, k := range Retired {
		if slices.Contains(Kinds, k) {
			t.Errorf("%s is both offered and retired", k)
		}
	}
}

func TestOrderNotesAndCanonicalOps(t *testing.T) {
	d := New()
	for _, op := range []Op{
		{Name: Frame, ID: "fruit", Label: "Fruit"},
		{Name: AddNode, ID: "pear", Kind: Idea, Label: "Pear", Group: "fruit"},
		{Name: AddNode, ID: "apple", Kind: Idea, Label: "apple", Group: "fruit"},
		{Name: AddNode, ID: "cheap", Kind: Pro, Label: "Cheap", On: "apple"},
		{Name: AddNode, ID: "fig", Kind: Idea, Label: "Fig", Group: "fruit"},
		{Name: Connect, From: "pear", To: "fig", Label: "then"},
		{Name: Annotate, Target: "pear", Text: "ripe"},
		{Name: Sort, Group: "fruit", By: "alphabetical"},
		{Name: Sort, By: ByCustom, IDs: []string{"fig", "pear"}},
		{Name: Notes, Lines: []string{"- Decided: pears", "  "}},
		{Name: Notes, Lines: []string{"Sam: buy figs"}},
	} {
		op.normalize()
		if err := d.Apply(op); err != nil {
			t.Fatalf("%s: %v", op.Name, err)
		}
	}
	if got := d.Notes(); !slices.Equal(got, []string{"Decided: pears", "Sam: buy figs"}) {
		t.Fatalf("notes = %q", got)
	}
	cmp := d.Compare("fruit")
	ids := []string{"pear", "fig", "apple"}
	slices.SortFunc(ids, cmp)
	if !slices.Equal(ids, []string{"apple", "fig", "pear"}) {
		t.Fatalf("alpha = %v", ids)
	}
	ids = []string{"apple", "pear", "fig"}
	slices.SortFunc(ids, d.Compare(""))
	if !slices.Equal(ids, []string{"fig", "pear", "apple"}) {
		t.Fatalf("custom = %v", ids)
	}
	if d.Compare("nothing") != nil {
		t.Fatal("an unsorted target has a comparison")
	}

	// The canonical ops rebuild the same board.
	again, errs := Replay(d.Ops())
	if len(errs) > 0 {
		t.Fatalf("replay: %v", errs)
	}
	if a, b := d.Digest(), again.Digest(); a != b {
		t.Fatalf("digest changed:\n%s\n---\n%s", a, b)
	}

	if err := d.Apply(Op{Name: Sort, Group: "nope", By: ByAlpha}); err == nil {
		t.Fatal("sorting a missing group went through")
	}
	if err := d.Apply(Op{Name: Sort, By: "sideways"}); err == nil {
		t.Fatal("an unknown sort went through")
	}
	if err := d.Apply(Op{Name: Undo}); err == nil {
		t.Fatal("undo applied to a doc")
	}
	if err := d.Apply(Op{Name: Notes, Replace: true}); err != nil || len(d.Notes()) != 0 {
		t.Fatalf("clearing notes: %v, %q", err, d.Notes())
	}
	if err := d.Apply(Op{Name: Sort, Group: "fruit", By: "none"}); err != nil || d.Compare("fruit") != nil {
		t.Fatalf("unsorting: %v", err)
	}
}

func TestOpKinds(t *testing.T) {
	for _, n := range []string{PointAt, GetBoard, Undo, RevertTo, Speak, Transcript, Auto} {
		if (Op{Name: n}).Changes() {
			t.Errorf("%s changes the board", n)
		}
	}
	for _, n := range []string{AddNode, Sort, Notes} {
		if !(Op{Name: n}).Changes() {
			t.Errorf("%s doesn't change the board", n)
		}
	}
	if !(Op{Name: Undo}).Versioned() || (Op{Name: AddNode}).Versioned() {
		t.Error("Versioned is wrong")
	}
	op, err := Decode(Notes, map[string]any{"lines": "Ship it"})
	if err != nil || !slices.Equal(op.Lines, []string{"Ship it"}) {
		t.Fatalf("lines as a string: %+v, %v", op, err)
	}
	op, err = Decode(RevertTo, map[string]any{"version": float64(3)})
	if err != nil || op.Version != 3 {
		t.Fatalf("revert_to: %+v, %v", op, err)
	}
	op, _ = Decode(Sort, map[string]any{"group": "board", "by": "Chronological"})
	if op.Group != "" || op.By != ByChrono {
		t.Fatalf("order: %+v", op)
	}
}
