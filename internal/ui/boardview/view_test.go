package boardview

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/measure"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
	"github.com/anthonyrego/huddle/internal/ui"
)

// fakeText measures text as fixed-width characters, wrapping at maxWidth,
// so layout tests run without fonts.
type fakeText struct{}

func (fakeText) Layout(runs []text.Run, maxWidth float32) text.Layout {
	var out text.Layout
	for _, r := range runs {
		cw := r.Style.Size * 0.55
		w := cw * float32(utf8.RuneCountInString(r.Text))
		lines := float32(1)
		if maxWidth > 0 && w > maxWidth {
			lines = float32(math.Ceil(float64(w / maxWidth)))
			w = maxWidth
		}
		out.Width = max(out.Width, w)
		out.Height += lines * r.Style.Size * 1.3
	}
	return out
}

func readOps(t *testing.T, name string) []board.Op {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ops []board.Op
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		op, err := board.ParseOp(sc.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ops = append(ops, op)
	}
	return ops
}

func ctxAt(now time.Time) *ui.Ctx {
	return &ui.Ctx{Text: fakeText{}, Pal: theme.For(false), Scale: 2, Now: now}
}

func center(r draw.Rect) draw.Point { return draw.Point{X: r.X + r.W/2, Y: r.Y + r.H/2} }

// TestEveryKindHasATag: a card's tag names its kind for people who don't
// know the kinds, retired ones included (saved boards still use them).
func TestEveryKindHasATag(t *testing.T) {
	for _, k := range slices.Concat(board.Kinds, board.Retired) {
		if measure.Tags[k] == "" {
			t.Errorf("no tag for %s", k)
		}
	}
}

// TestLayoutStaysStill is the S1 layout gate: as a workflow is drawn box by
// box, the boxes already there barely move.
func TestLayoutStaysStill(t *testing.T) {
	for _, name := range []string{"orders.jsonl", "report.jsonl", "onboarding.jsonl", "trip.jsonl"} {
		t.Run(name, func(t *testing.T) {
			ops := readOps(t, name)
			doc := board.New()
			v := New()
			v.SetViewport(800)
			now := time.Unix(1000, 0)
			v.Measure(ctxAt(now), 1200)
			var moves []float64
			for i, op := range ops {
				before := map[string]draw.Rect{}
				for id, r := range v.Layout().Boxes {
					before[id] = r.Rect()
				}
				if err := doc.Apply(op); err != nil {
					t.Fatalf("op %d (%s %s): %v", i+1, op.Name, op.ID, err)
				}
				adds := op.Name == board.AddNode && op.On == "" && before[op.ID] == (draw.Rect{})
				now = now.Add(time.Second)
				v.SetDoc(doc, now)
				v.Measure(ctxAt(now), 1200)
				if !adds || len(before) == 0 {
					continue
				}
				var ds []float64
				for id, r := range v.Layout().Boxes {
					if b, ok := before[id]; ok {
						a, c := center(b), center(r.Rect())
						ds = append(ds, math.Hypot(float64(a.X-c.X), float64(a.Y-c.Y)))
					}
				}
				slices.Sort(ds)
				moves = append(moves, ds[len(ds)/2])
			}
			slices.Sort(moves)
			med, worst := moves[len(moves)/2], moves[len(moves)-1]
			t.Logf("%d adds: median movement %.1f, worst add %.1f", len(moves), med, worst)
			if med >= 40 {
				t.Errorf("median movement of existing boxes per add = %.1f, want < 40", med)
			}
			assertNoOverlaps(t, v)
		})
	}
}

// assertNoOverlaps checks that no two boxes (with their satellites)
// overlap, and logs any frame that covers a box outside it.
func assertNoOverlaps(t *testing.T, v *View) {
	t.Helper()
	lo := v.Layout()
	ids := make([]string, 0, len(lo.Boxes))
	for id := range lo.Boxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if !lo.Boxes[a].Rect().Intersect(lo.Boxes[b].Rect()).Empty() {
				t.Errorf("boxes %s and %s overlap: %+v %+v", a, b, lo.Boxes[a], lo.Boxes[b])
			}
		}
	}
	now := v.moveAt.Add(time.Hour)
	for _, g := range v.doc.Groups() {
		r, ok := v.groupRect(g.ID, now)
		if !ok {
			continue
		}
		for _, id := range ids {
			if v.doc.Node(id).Group == g.ID {
				continue
			}
			if !r.Intersect(lo.Boxes[id].Rect()).Empty() {
				t.Errorf("frame %s covers %s, which isn't in it", g.ID, id)
			}
		}
	}
}

func TestHitAndLocateAgree(t *testing.T) {
	doc, errs := board.Replay(readOps(t, "orders.jsonl"))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	v := New()
	v.SetViewport(800)
	now := time.Unix(1000, 0)
	c := ctxAt(now)
	v.SetDoc(doc, now)
	v.Measure(c, 1200)
	c.Now = now.Add(time.Hour) // after every glide
	for _, id := range []string{"copy_rows", "ai_rows", "monday"} {
		r, ok := v.Locate(c, ui.Part{Kind: ui.PartNode, ID: id})
		if !ok {
			t.Fatalf("locate %s failed", id)
		}
		// A point just inside the top-left corner, so a frame's own label
		// area is hit rather than a card inside it.
		p := draw.Point{X: r.X + 4, Y: r.Y + 4}
		part, ok := v.Hit(c, p)
		if !ok || part.ID != id {
			t.Errorf("hit at %s's corner = %q, %v", id, part.ID, ok)
		}
	}
}

// TestResetShowsAnotherBoard: nothing glides over from the board before,
// the new one fades in, and the camera fits it at once.
func TestResetShowsAnotherBoard(t *testing.T) {
	orders, _ := board.Replay(readOps(t, "orders.jsonl"))
	report, _ := board.Replay(readOps(t, "report.jsonl"))
	v := New()
	v.SetViewport(800)
	now := time.Unix(1000, 0)
	v.SetDoc(orders, now)
	v.Measure(ctxAt(now), 1200)
	v.ZoomBy(2, draw.Point{X: 100, Y: 100}, now) // the user zoomed in: not fitted

	now = now.Add(time.Minute)
	v.Reset(report, now)
	v.Measure(ctxAt(now), 1200)
	for id := range v.Layout().Boxes {
		if report.Node(id) == nil {
			t.Fatalf("%s is from the board before", id)
		}
		if _, ok := v.from[id]; ok {
			t.Fatalf("%s glides from the board before", id)
		}
		if b, ok := v.born[id]; !ok || b.Before(now) {
			t.Fatalf("%s doesn't fade in", id)
		}
	}
	s, off := v.camera(now)
	if fs, foff := v.fitTarget(); s != fs || off != foff {
		t.Fatalf("camera %v %v isn't the fit", s, off)
	}
	if !v.fitted {
		t.Fatal("the new board isn't fitted")
	}
}
