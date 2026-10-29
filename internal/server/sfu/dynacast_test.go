package sfu

// Dynacast end to end on loopback: a publisher's synthetic camera that
// pauses the layers the SFU says nobody wants (and starts them again with
// a keyframe, as the room's sender does), and viewers who come, resize,
// turn off and leave.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// shortFirst has videos published during the test keep every layer only
// dynacastShrink after the publish, for tests not about that.
func shortFirst(t *testing.T) {
	was := dynacastFirst
	dynacastFirst = dynacastShrink
	t.Cleanup(func() { dynacastFirst = was })
}

// follow has the camera pause the layers c hears nobody wants, and start
// them again with a keyframe, as the room's allocator and sender do.
func (cam *camera) follow(c *testClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onWanted = func(w signal.Wanted) {
		if w.SID != cam.sid {
			return
		}
		cam.mu.Lock()
		defer cam.mu.Unlock()
		for i, l := range camLayers {
			want := slices.Contains(w.RIDs, l.RID)
			if want && cam.paused[i] {
				cam.ask[i] = true
			}
			cam.paused[i] = !want
		}
	}
}

// wantedHeard is a copy of the wanted c has heard.
func (c *testClient) wantedHeard() []wantedSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.wanted)
}

// waitWanted waits until the newest wanted c has heard for sid is rids
// (largest first), and returns when it came.
func waitWanted(t *testing.T, c *testClient, sid string, rids ...string) time.Time {
	t.Helper()
	var at time.Time
	waitFor(t, wait+dynacastFirst, c.id+" to hear wanted "+fmtRIDs(rids), func() bool {
		ws := c.wantedHeard()
		for i := len(ws) - 1; i >= 0; i-- {
			if ws[i].sid == sid {
				at = ws[i].at
				return slices.Equal(ws[i].rids, rids)
			}
		}
		return false
	})
	return at
}

func fmtRIDs(rids []string) string { return "[" + strings.Join(rids, " ") + "]" }

// firstUnit is the first unit of layer rid c was sent of who's camera
// after at.
func firstUnit(c *testClient, who, rid string, at time.Time) (unit, bool) {
	for _, u := range since(c.unitsFrom(who), at) {
		if camLayers[u.layer].RID == rid {
			return u, true
		}
	}
	return unit{}, false
}

// The wanted set follows viewers: nothing while nobody watches (every
// layer paused dynacastFirst after the publish), a layer added at once
// when a viewer asks for it, one dropped dynacastShrink after the last
// viewer wanting or sent it stops, nothing again once everyone has gone.
// A viewer of a camera nobody watched gets its first frame within a few
// frames of asking.
func TestDynacastWanted(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	published := time.Now()
	at := waitWanted(t, a, cam.sid)
	t.Logf("nobody watching: every layer paused %v after the publish", at.Sub(published).Round(time.Millisecond))
	if d := at.Sub(published); d < dynacastFirst-tick || d > dynacastFirst+3*tick {
		t.Errorf("every layer paused %v after the publish, want %v", d, dynacastFirst)
	}
	time.Sleep(layerQuiet) // nothing comes: every layer paused alike

	b := ts.join(t, "r", "b", sfuclient.Options{})
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	asked := time.Now()
	b.settings(t, "a", 320, 180, true)
	at = waitWanted(t, a, cam.sid, "q")
	waitUnits(t, b, "a", "q", 10)
	first, _ := firstUnit(b, "a", "q", asked)
	t.Logf("b at 320x180: wanted [q] %v after asking; its first frame (%v) %v after", at.Sub(asked).Round(100*time.Microsecond), first, first.at.Sub(asked).Round(time.Millisecond))
	if d := at.Sub(asked); d > 100*time.Millisecond {
		t.Errorf("wanted [q] %v after b asked", d)
	}
	if d := first.at.Sub(asked); !first.key || d > 300*time.Millisecond {
		t.Errorf("b's first frame %v, %v after asking", first, d)
	}

	c := ts.join(t, "r", "c", sfuclient.Options{})
	waitFor(t, wait, "c to be offered a's camera", func() bool { return c.from("a", signal.Video) != nil })
	asked = time.Now()
	c.settings(t, "a", 1280, 720, true)
	at = waitWanted(t, a, cam.sid, "f", "q")
	waitUnits(t, c, "a", "f", 10)
	t.Logf("c at 1280x720: wanted [f q] %v after asking", at.Sub(asked).Round(100*time.Microsecond))

	// c shrinks to 360p: h joins at once; f, still sent until c's switch
	// to h, leaves dynacastShrink after it.
	asked = time.Now()
	c.settings(t, "a", 640, 360, true)
	at = waitWanted(t, a, cam.sid, "f", "h", "q")
	onH, _ := func() (unit, bool) {
		var u unit
		var ok bool
		waitFor(t, wait, "c on h", func() bool { u, ok = firstUnit(c, "a", "h", asked); return ok })
		return u, ok
	}()
	dropped := waitWanted(t, a, cam.sid, "h", "q")
	t.Logf("c at 640x360: wanted [f h q] %v after asking, on h %v after, f dropped %v after it was last sent", at.Sub(asked).Round(100*time.Microsecond),
		onH.at.Sub(asked).Round(time.Millisecond), dropped.Sub(onH.at).Round(time.Millisecond))
	if d := dropped.Sub(onH.at); d < dynacastShrink-2*tick || d > dynacastShrink+3*tick {
		t.Errorf("f dropped %v after c's switch to h, want %v", d, dynacastShrink)
	}

	// c turns off, then b leaves.
	off := time.Now()
	c.settings(t, "a", 640, 360, false)
	at = waitWanted(t, a, cam.sid, "q")
	t.Logf("c off: h dropped %v after", at.Sub(off).Round(time.Millisecond))
	if d := at.Sub(off); d < dynacastShrink-tick {
		t.Errorf("h dropped %v after c turned off", d)
	}
	left := time.Now()
	b.s.Close()
	at = waitWanted(t, a, cam.sid)
	t.Logf("b gone: q dropped %v after", at.Sub(left).Round(time.Millisecond))
	checkVideo(t, "b", b.unitsFrom("a"))
	checkVideo(t, "c", c.unitsFrom("a"))
	for _, w := range a.wantedHeard() {
		t.Logf("  wanted %v at %v", fmtRIDs(w.rids), w.at.Sub(published).Round(time.Millisecond))
	}
}

// Every layer stays wanted for dynacastFirst after the publish, whatever
// viewers want meanwhile: a viewer of the top layer moving to the smallest
// a second in doesn't cut that short.
func TestDynacastFirstHold(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	published := time.Now()
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 1280, 720, true)
	waitUnits(t, b, "a", "f", 10)
	moved := time.Now()
	b.settings(t, "a", 320, 180, true)
	at := waitWanted(t, a, cam.sid, "q")
	t.Logf("b at 1280x720, then 320x180 %v in: wanted [q] %v after the publish", moved.Sub(published).Round(time.Millisecond), at.Sub(published).Round(time.Millisecond))
	if d := at.Sub(published); d < dynacastFirst-tick {
		t.Errorf("wanted [q] %v after the publish, before %v", d, dynacastFirst)
	}
	if ws := a.wantedHeard(); len(ws) != 1 {
		t.Errorf("wanted %d times", len(ws))
	}
}

// A tile that shrinks and grows back within dynacastShrink never stops the
// larger layer's encoder: the publisher hears a set without it only once
// it has been out of the set that long.
func TestDynacastShrinkDelay(t *testing.T) {
	shortFirst(t)
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 1280, 720, true)
	start := waitWanted(t, a, cam.sid, "f")
	waitUnits(t, b, "a", "f", 10)
	for range 3 {
		b.settings(t, "a", 320, 180, true)
		time.Sleep(700 * time.Millisecond)
		b.settings(t, "a", 1280, 720, true)
		time.Sleep(700 * time.Millisecond)
	}
	waitWanted(t, a, cam.sid, "f")
	time.Sleep(dynacastShrink + 3*tick)
	var sets []string
	for _, w := range a.wantedHeard() {
		if w.at.Before(start) {
			continue
		}
		sets = append(sets, fmtRIDs(w.rids))
		if !slices.Contains(w.rids, "f") {
			t.Errorf("wanted %v at %v: f dropped while the tile came back to 720p", fmtRIDs(w.rids), w.at.Sub(start).Round(time.Millisecond))
		}
	}
	t.Logf("three trips to 180p and back, 0.7 s each way: wanted %v", sets)
	if last := a.wantedHeard(); !slices.Equal(last[len(last)-1].rids, []string{"f"}) {
		t.Errorf("wanted %v at the end", fmtRIDs(last[len(last)-1].rids))
	}
	checkVideo(t, "b", b.unitsFrom("a"))
}

// A layer paused for nobody wanting it, wanted again: the publisher hears
// at once, the viewer is sent the nearest running layer meanwhile, and
// switches up at the layer's first keyframe.
func TestDynacastPausedLayerWanted(t *testing.T) {
	shortFirst(t)
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 320, 180, true)
	waitWanted(t, a, cam.sid, "q")
	pub := ts.s.room("r").participant("a").publication(cam.sid)
	waitFor(t, wait, "f and h counted paused", func() bool { return !pub.layers[0].alive.Load() && !pub.layers[1].alive.Load() })
	for range 2 {
		asked := time.Now()
		b.settings(t, "a", 1280, 720, true)
		at := waitWanted(t, a, cam.sid, "f", "q")
		var onF unit
		waitFor(t, wait, "b on f", func() bool { u, ok := firstUnit(b, "a", "f", asked); onF = u; return ok })
		t.Logf("b to 1280x720: wanted [f q] %v after asking, on f (%v) %v after", at.Sub(asked).Round(100*time.Microsecond), onF, onF.at.Sub(asked).Round(time.Millisecond))
		if d := onF.at.Sub(asked); !onF.key || d > 150*time.Millisecond {
			t.Errorf("b on f %v after asking, with %v", d, onF)
		}
		waitWanted(t, a, cam.sid, "f")
		b.settings(t, "a", 320, 180, true)
		waitWanted(t, a, cam.sid, "f", "q")
		waitWanted(t, a, cam.sid, "q")
		waitFor(t, wait, "f counted paused", func() bool { return !pub.layers[0].alive.Load() })
	}
	us := b.unitsFrom("a")
	checkVideo(t, "b", us)
	path, n := layersOf(us)
	t.Logf("b was sent %s %v; keyframes asked %v", path, n, cam.keyRequests())
}

// Many viewers: the set is the union of what they want (a viewer that
// never says a size wants the top layer, one turned off nothing), and it
// shrinks as they go.
func TestDynacastManyViewers(t *testing.T) {
	shortFirst(t)
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	waitWanted(t, a, cam.sid) // nobody yet
	sizes := []struct {
		w, h int
		rid  string // what it's sent ("" nothing)
	}{
		{1280, 720, "f"}, {320, 180, "q"}, {160, 90, "q"}, {320, 180, "q"},
		{342, 192, "h"}, {1920, 1080, "f"}, {0, 0, ""}, {-1, -1, "f"}, // the last never says
	}
	var vs []*testClient
	for i := range sizes {
		v := ts.join(t, "r", "v"+string(rune('0'+i)), sfuclient.Options{})
		vs = append(vs, v)
	}
	for i, v := range vs {
		waitFor(t, wait, v.id+" to be offered a's camera", func() bool { return v.from("a", signal.Video) != nil })
		if s := sizes[i]; s.w >= 0 {
			v.settings(t, "a", s.w, s.h, true)
		}
	}
	waitWanted(t, a, cam.sid, "f", "h", "q")
	for i, v := range vs {
		if rid := sizes[i].rid; rid != "" {
			waitUnits(t, v, "a", rid, 15)
			if path, _ := layersOf(v.unitsFrom("a")); path[0:1] != rid {
				t.Errorf("%s (%dx%d): sent %s", v.id, sizes[i].w, sizes[i].h, path)
			}
			checkVideo(t, v.id, v.unitsFrom("a"))
		} else if n := len(v.videoFrom("a")); n != 0 {
			t.Errorf("%s (off): sent %d packets", v.id, n)
		}
	}
	// The top layer's viewers go: f leaves.
	for _, i := range []int{0, 5, 7} {
		vs[i].s.Close()
	}
	gone := time.Now()
	at := waitWanted(t, a, cam.sid, "h", "q")
	t.Logf("f's viewers gone: wanted [h q] %v after", at.Sub(gone).Round(time.Millisecond))
	// The smallest layer's turn off: q leaves.
	for _, i := range []int{1, 2, 3} {
		vs[i].settings(t, "a", 320, 180, false)
	}
	waitWanted(t, a, cam.sid, "h")
	vs[4].s.Close()
	waitWanted(t, a, cam.sid)
	var sets []string
	for _, w := range a.wantedHeard() {
		sets = append(sets, fmtRIDs(w.rids))
	}
	t.Logf("wanted %v", sets)
}

// A camera with only the whiteboard (in-process, sent voices alone) in
// the room: nobody is sent it, and every layer pauses.
func TestDynacastWhiteboardOnly(t *testing.T) {
	ts := newTestSFU(t, nil)
	wb, err := ts.s.LocalJoin(rtctoken.Grant{Room: "r", Identity: "whiteboard", Name: "Whiteboard", Agent: true}, LocalEvents{})
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Leave()
	a := ts.join(t, "r", "a", sfuclient.Options{})
	cam := publishCamera(t, a)
	cam.follow(a)
	published := time.Now()
	at := waitWanted(t, a, cam.sid)
	t.Logf("a camera and the whiteboard: every layer paused %v after the publish", at.Sub(published).Round(time.Millisecond))
	r := ts.s.room("r")
	r.mu.Lock()
	downs := len(r.parts["whiteboard"].downs)
	r.mu.Unlock()
	if downs != 0 {
		t.Errorf("the whiteboard is sent %d tracks", downs)
	}
	time.Sleep(500 * time.Millisecond)
	cam.mu.Lock()
	paused := cam.paused
	cam.mu.Unlock()
	if paused != [3]bool{true, true, true} {
		t.Errorf("layers paused: %v", paused)
	}
}
