package transcript

import (
	"fmt"
	"math"
	"testing"
	"time"
	"unicode/utf8"

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

func ctx() *ui.Ctx {
	return &ui.Ctx{Text: fakeText{}, Pal: theme.For(false), Scale: 2, Now: time.Unix(1000, 0)}
}

// turns is n entries, alternating speakers, each a few lines long.
func turns(n int) []Entry {
	var es []Entry
	for i := range n {
		who := "You"
		if i%2 == 1 {
			who = "AI"
		}
		es = append(es, Entry{Who: who, Text: fmt.Sprintf("turn %d: every Monday I copy the orders from the shared inbox into the sheet", i)})
	}
	return es
}

func measure(v *View) { v.Measure(ctx(), 340) }

func TestFollowsTheNewestUntilScrolledUp(t *testing.T) {
	v := New()
	v.SetViewport(400)
	v.SetEntries(turns(12))
	measure(v)
	if v.maxScroll() <= 0 {
		t.Fatalf("12 turns fit in 400: content %.0f", v.content)
	}
	if v.Scroll() != v.maxScroll() {
		t.Fatalf("scroll %.0f, want the end %.0f", v.Scroll(), v.maxScroll())
	}

	v.SetEntries(turns(13))
	measure(v)
	if v.Scroll() != v.maxScroll() {
		t.Fatalf("after a new turn scroll %.0f, want the end %.0f", v.Scroll(), v.maxScroll())
	}

	// Scrolling up stops following: a new turn leaves the view where it is.
	v.Event(ctx(), ui.Event{Kind: ui.Wheel, DY: -120})
	if v.Following() {
		t.Fatal("still following after scrolling up")
	}
	at := v.Scroll()
	v.SetEntries(turns(14))
	measure(v)
	if v.Scroll() != at {
		t.Fatalf("a new turn moved the view from %.0f to %.0f", at, v.Scroll())
	}

	// Back at the end, it follows again.
	v.Event(ctx(), ui.Event{Kind: ui.Wheel, DY: 1e6})
	if !v.Following() || v.Scroll() != v.maxScroll() {
		t.Fatalf("at the end: following %v, scroll %.0f of %.0f", v.Following(), v.Scroll(), v.maxScroll())
	}

	// The top is the limit.
	v.Event(ctx(), ui.Event{Kind: ui.Wheel, DY: -1e6})
	if v.Scroll() != 0 {
		t.Fatalf("scrolled past the top: %.0f", v.Scroll())
	}
}

func TestShortTranscriptDoesNotScroll(t *testing.T) {
	v := New()
	v.SetViewport(800)
	v.SetEntries(turns(2))
	measure(v)
	v.Event(ctx(), ui.Event{Kind: ui.Wheel, DY: 50})
	if v.Scroll() != 0 || !v.Following() {
		t.Fatalf("scroll %.0f, following %v", v.Scroll(), v.Following())
	}
}

func TestOneLabelPerSpeakerRun(t *testing.T) {
	v := New()
	v.SetViewport(800)
	v.SetEntries([]Entry{
		{Who: "You", Text: "first"},
		{Who: "You", Text: "more, said later", Live: true},
		{Who: "AI", Text: "a reply"},
	})
	measure(v)
	var got []bool
	for _, el := range v.lays {
		got = append(got, el.label)
	}
	if fmt.Sprint(got) != "[true false true]" {
		t.Fatalf("labels %v, want [true false true]", got)
	}
}

func TestRelayoutOnlyWhatChanged(t *testing.T) {
	v := New()
	v.SetViewport(800)
	es := turns(3)
	v.SetEntries(es)
	measure(v)
	es = append(es[:2:2], Entry{Who: "You", Text: "a longer line now that more was said", Live: true})
	v.SetEntries(es)
	measure(v)
	if v.lays[2].e != es[2] {
		t.Fatalf("the changed entry kept %+v", v.lays[2].e)
	}
	if v.lays[1].y+v.lays[1].h >= v.lays[2].y {
		t.Fatalf("entries overlap: %+v then %+v", v.lays[1], v.lays[2])
	}
}

func TestAMarkStartsALabelRun(t *testing.T) {
	v := New()
	v.SetViewport(800)
	v.SetEntries([]Entry{
		{Who: "You", Text: "said before"},
		{Text: "today 15:32", Mark: true},
		{Who: "You", Text: "said after"},
	})
	measure(v)
	var got []bool
	for _, el := range v.lays {
		got = append(got, el.label)
	}
	if fmt.Sprint(got) != "[true false true]" {
		t.Fatalf("labels %v, want [true false true]", got)
	}
	if v.lays[1].y <= v.lays[0].y+v.lays[0].h || v.lays[2].y <= v.lays[1].y+v.lays[1].h {
		t.Fatalf("entries overlap: %+v", v.lays)
	}
}
