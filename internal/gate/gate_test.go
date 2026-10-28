package gate

import (
	"strings"
	"testing"
)

var names = []string{"whiteboard"}

func TestWake(t *testing.T) {
	for _, c := range []struct {
		text, rest string
		ok         bool
	}{
		{"Whiteboard, draw our release process: build, then test, then deploy.", "draw our release process: build, then test, then deploy.", true},
		{"White board, add a code review step between test and deploy.", "add a code review step between test and deploy.", true},
		{"whiteboard draw the flow", "draw the flow", true},
		{"OK whiteboard, sort the options alphabetically.", "sort the options alphabetically.", true},
		{"So, whiteboard, what's the riskiest step?", "what's the riskiest step?", true},
		{"Hey, white bored, add a box.", "add a box.", true},
		{"Whiteboards, add a box.", "add a box.", true},
		{"Wide board, undo that.", "undo that.", true},
		{"Can you draw that as a flow, whiteboard?", "Can you draw that as a flow?", true},
		{"Draw that, whiteboard, as a flow.", "Draw that as a flow.", true},
		{"Whiteboard.", "", true},
		{"Hey whiteboard", "", true},

		{"I'll put it on the whiteboard later, after lunch.", "", false},
		{"I'll put it on the whiteboard later.", "", false},
		{"Can you share the whiteboard link?", "", false},
		{"This whiteboard thing is pretty neat.", "", false},
		{"We drew it on a whiteboard last week.", "", false},
		{"Whiteboard markers are out again.", "", false},
		{"Whiteboard is down for me.", "", false},
		{"Let's ask the whiteboard to draw it later.", "", false},
		{"Let's use the whiteboard for that.", "", false},
		{"My keyboard is broken.", "", false},
		{"The dashboard shows it.", "", false},
		{"Write it on the board.", "", false},
	} {
		rest, ok := Wake(c.text, names)
		if ok != c.ok || rest != c.rest {
			t.Errorf("Wake(%q) = %q, %v; want %q, %v", c.text, rest, ok, c.rest, c.ok)
		}
	}
}

func TestStopAndCourtesy(t *testing.T) {
	for _, s := range []string{"stop", "Stop.", "stop talking", "OK, stop!", "never mind"} {
		if !IsStop(s) {
			t.Errorf("%q isn't a stop", s)
		}
	}
	for _, s := range []string{"stop the deploy step from failing", "draw a stop sign"} {
		if IsStop(s) {
			t.Errorf("%q is a stop", s)
		}
	}
	if !Courtesy("Thanks.") || Courtesy("thanks, add a box") {
		t.Error("courtesy is wrong")
	}
}

// run feeds a script of events to a gate, ticking every 100 ms, and
// returns the commands in the order they came out.
type ev struct {
	at       int64
	who      string
	text     string // a final segment (interim with ~ prefix)
	start    int64
	typed    bool
	key      int // 1 down, -1 up
	speaking int // 1 on, -1 off
}

func run(t *testing.T, evs []ev, until int64) ([]Command, []Out) {
	t.Helper()
	g := New(Config{})
	var cmds []Command
	var all []Out
	i := 0
	for now := int64(0); now <= until; now += 100 {
		for ; i < len(evs) && evs[i].at <= now; i++ {
			e := evs[i]
			var outs []Out
			switch {
			case e.speaking != 0:
				g.SetSpeaking(e.speaking > 0)
			case e.typed:
				outs = g.Typed(e.who, strings.ToUpper(e.who[:1])+e.who[1:], e.text, e.at)
			case e.key != 0:
				outs = g.Address(e.who, e.who, e.key > 0, e.at)
			default:
				final := !strings.HasPrefix(e.text, "~")
				outs = g.Segment(Segment{Identity: e.who, Speaker: e.who, Text: strings.TrimPrefix(e.text, "~"), StartMs: e.start, EndMs: e.at, Final: final})
			}
			all = append(all, outs...)
		}
		all = append(all, g.Tick(now)...)
	}
	for _, o := range all {
		if o.Kind == Done {
			cmds = append(cmds, o.Command)
		}
	}
	return cmds, all
}

func TestCommandEndsAfterAPause(t *testing.T) {
	cmds, _ := run(t, []ev{
		{at: 1000, who: "sam", text: "~Whiteboard draw", start: 0},
		{at: 2500, who: "sam", text: "Whiteboard, draw our release process.", start: 0},
		// A short pause, then more of the same command.
		{at: 3000, who: "sam", text: "~", start: 3000}, // the ears hear speech start
		{at: 3600, who: "sam", text: "~Build test", start: 3000},
		{at: 4200, who: "sam", text: "Build, test, deploy.", start: 3000},
		{at: 6000, who: "kim", text: "Sounds good to me.", start: 5000},
	}, 8000)
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v", cmds)
	}
	c := cmds[0]
	if c.Text != "draw our release process. Build, test, deploy." || c.StartMs != 0 || c.EndMs != 4200 || c.Source != Spoken {
		t.Fatalf("command = %+v", c)
	}
	if c.AtMs < 4200+700 || c.AtMs > 4200+800 {
		t.Fatalf("let through at %d, want 700-800 ms after the end", c.AtMs)
	}
}

func TestNameAloneWaitsForTheCommand(t *testing.T) {
	cmds, outs := run(t, []ev{
		{at: 800, who: "dan", text: "Whiteboard?", start: 0},
		{at: 3000, who: "dan", text: "Put the budget in a box.", start: 2000},
	}, 6000)
	if len(cmds) != 1 || cmds[0].Text != "Put the budget in a box." {
		t.Fatalf("commands = %+v", cmds)
	}
	if outs[0].Kind != Listening {
		t.Fatalf("first out = %+v, want listening", outs[0])
	}
	// The name and then nothing: dropped.
	cmds, outs = run(t, []ev{{at: 800, who: "dan", text: "Whiteboard.", start: 0}}, 6000)
	if len(cmds) != 0 || outs[len(outs)-1].Kind != Dropped {
		t.Fatalf("outs = %+v", outs)
	}
}

func TestDistractorsOpenNothing(t *testing.T) {
	cmds, outs := run(t, []ev{
		{at: 1000, who: "kim", text: "~I'll put it on the whiteboard", start: 0},
		{at: 2000, who: "kim", text: "I'll put it on the whiteboard later.", start: 0},
		{at: 4000, who: "sam", text: "Whiteboard markers are out again.", start: 3000},
		{at: 6000, who: "dan", text: "Thanks, whiteboard.", start: 5000},
	}, 9000)
	if len(cmds) != 0 {
		t.Fatalf("commands = %+v", cmds)
	}
	for _, o := range outs {
		if o.Kind == Listening && o.Identity != "dan" {
			t.Fatalf("opened for %+v", o)
		}
	}
}

func TestSpeakingIgnoresTheNameButHearsStop(t *testing.T) {
	cmds, _ := run(t, []ev{
		{at: 100, speaking: 1},
		{at: 1000, who: "sam", text: "Whiteboard, add a box.", start: 0},
		{at: 2500, who: "kim", text: "Whiteboard, stop.", start: 2000},
		{at: 2600, speaking: -1},
		{at: 4000, who: "sam", text: "Whiteboard, add a box.", start: 3000},
	}, 6000)
	if len(cmds) != 2 || !cmds[0].Stop || cmds[0].Speaker != "kim" || cmds[1].Text != "add a box." {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].AtMs != 2500 {
		t.Fatalf("stop let through at %d, want at once", cmds[0].AtMs)
	}
}

func TestTypedAndKey(t *testing.T) {
	cmds, _ := run(t, []ev{
		{at: 500, who: "ann", text: "whiteboard, sort it alphabetically", typed: true},
		{at: 1000, who: "bob", key: 1},
		{at: 2500, who: "bob", text: "~Add a step", start: 1200},
		{at: 2600, who: "bob", key: -1},
		{at: 3100, who: "bob", text: "Add a step called review.", start: 1200},
	}, 6000)
	if len(cmds) != 2 {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].Source != Typed || cmds[0].Text != "sort it alphabetically" || cmds[0].Speaker != "Ann" {
		t.Fatalf("typed = %+v", cmds[0])
	}
	if cmds[1].Source != Key || cmds[1].Text != "Add a step called review." || cmds[1].AtMs > 3300 {
		t.Fatalf("key = %+v", cmds[1])
	}
	// The key with nothing said: dropped after the grace.
	cmds, outs := run(t, []ev{{at: 1000, who: "bob", key: 1}, {at: 1500, who: "bob", key: -1}}, 5000)
	if len(cmds) != 0 || outs[len(outs)-1].Kind != Dropped {
		t.Fatalf("outs = %+v", outs)
	}
}

func TestTwoAtOnceComeOutInOrder(t *testing.T) {
	cmds, _ := run(t, []ev{
		{at: 2200, who: "kim", text: "Whiteboard, add a box for hiring.", start: 300},
		{at: 2200, who: "sam", text: "Whiteboard, add a box for budget.", start: 0},
	}, 5000)
	if len(cmds) != 2 || cmds[0].Speaker != "sam" || cmds[1].Speaker != "kim" {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].ID != "c1" || cmds[1].ID != "c2" {
		t.Fatalf("ids = %s, %s", cmds[0].ID, cmds[1].ID)
	}
}

func TestOldestOpen(t *testing.T) {
	g := New(Config{})
	if _, ok := g.OldestOpen(); ok {
		t.Fatal("open with nothing said")
	}
	g.Segment(Segment{Identity: "kim", Speaker: "Kim", Text: "Whiteboard, add a con", StartMs: 300, EndMs: 2000, Final: true})
	g.Segment(Segment{Identity: "sam", Speaker: "Sam", Text: "Whiteboard, add a pro", StartMs: 100, EndMs: 1500, Final: true})
	if s, ok := g.OldestOpen(); !ok || s != 100 {
		t.Fatalf("oldest = %d, %v", s, ok)
	}
}

func TestInterimNameCountsAsOpen(t *testing.T) {
	g := New(Config{})
	outs := g.Segment(Segment{Identity: "kim", Speaker: "Kim", Text: "Whiteboard add a", StartMs: 300, EndMs: 900})
	if len(outs) != 1 || outs[0].Kind != Listening {
		t.Fatalf("outs = %+v", outs)
	}
	if outs := g.Segment(Segment{Identity: "kim", Speaker: "Kim", Text: "Whiteboard add a con", StartMs: 300, EndMs: 1200}); len(outs) != 0 {
		t.Fatalf("listening twice: %+v", outs)
	}
	if s, ok := g.OldestOpen(); !ok || s != 300 || g.Open()[0] != "kim" {
		t.Fatalf("oldest = %d, %v; open %v", s, ok, g.Open())
	}
	g.Segment(Segment{Identity: "kim", Speaker: "Kim", Text: "Whiteboard, add a con.", StartMs: 300, EndMs: 2000, Final: true})
	if s, ok := g.OldestOpen(); !ok || s != 300 {
		t.Fatalf("after the final: %d, %v", s, ok)
	}
	g.Tick(3000)
	if _, ok := g.OldestOpen(); ok {
		t.Fatal("still open after the command")
	}
	// An interim with the name whose final never comes stops counting.
	g.Segment(Segment{Identity: "dan", Speaker: "Dan", Text: "whiteboard", StartMs: 4000, EndMs: 4500})
	g.Tick(4500 + pendingMs + 100)
	if _, ok := g.OldestOpen(); ok {
		t.Fatal("a lost interim stays open")
	}
}

func TestWakeAtTheEndOfAQuestion(t *testing.T) {
	for _, c := range []struct {
		text, rest string
		ok         bool
	}{
		{"How many steps are on the board now whiteboard?", "How many steps are on the board now?", true},
		{"What comes after review, whiteboard?", "What comes after review?", true},
		{"Do we have a new whiteboard?", "", false},
		{"Is that the whiteboard?", "", false},
		{"We need a bigger whiteboard.", "", false},
	} {
		rest, ok := Wake(c.text, names)
		if ok != c.ok || rest != c.rest {
			t.Errorf("Wake(%q) = %q, %v; want %q, %v", c.text, rest, ok, c.rest, c.ok)
		}
	}
}
