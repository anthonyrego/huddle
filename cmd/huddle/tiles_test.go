package main

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// The tiles run down the right while the stage keeps enough width, shrink
// to fit everyone, and fold into a row with a "+N" tile when they can't.
func TestLayoutStage(t *testing.T) {
	order := func(n int) []int {
		p := make([]int, n)
		for i := range p {
			p[i] = i
		}
		return p
	}
	sl := layoutStage(1200, 748, 0, nil)
	if !sl.strip.Empty() || sl.stage != (draw.Rect{W: 1200, H: 748}) {
		t.Fatalf("no one: strip %v stage %v", sl.strip, sl.stage)
	}

	sl = layoutStage(1200, 748, 4, order(4))
	if sl.row || len(sl.tiles) != 4 || len(sl.more) != 0 {
		t.Fatalf("4 at 1200: row %v, %d tiles, %d more", sl.row, len(sl.tiles), len(sl.more))
	}
	if w := sl.tiles[0].W; w != 216 || sl.stage.W != 1200-216-2*tilePad {
		t.Fatalf("4 at 1200: tiles %v wide, stage %v", w, sl.stage.W)
	}

	sl = layoutStage(1200, 748, 7, order(7))
	last := sl.tiles[len(sl.tiles)-1]
	if len(sl.tiles) != 7 || last.Bottom() > 748-tilePad+0.01 || sl.tiles[0].W < tileFloorW {
		t.Fatalf("7 at 1200x748: %d tiles, last ends %v, width %v", len(sl.tiles), last.Bottom(), sl.tiles[0].W)
	}
	for _, r := range sl.tiles {
		if d := r.W/r.H - tileAspect; d > 0.01 || d < -0.01 {
			t.Fatalf("a tile isn't 16:9: %v", r)
		}
	}

	// Short and wide: the column can't hold 7 even at the smallest.
	sl = layoutStage(1200, 300, 7, []int{0, 3, 1, 2, 4, 5, 6})
	if sl.row || sl.who[len(sl.who)-1] != -1 {
		t.Fatalf("7 at 1200x300: row %v, who %v", sl.row, sl.who)
	}
	if !slices.Contains(sl.who, 3) || slices.Contains(sl.who, 6) {
		t.Fatalf("prio 0,3,1… should keep 3 and drop 6: who %v more %v", sl.who, sl.more)
	}

	// The minimum window: a row along the top, centered, and the stage below.
	sl = layoutStage(640, 368, 7, order(7))
	if !sl.row || sl.stage.Y != sl.strip.H || sl.stage.H != 368-sl.strip.H {
		t.Fatalf("640: row %v strip %v stage %v", sl.row, sl.strip, sl.stage)
	}
	if n := len(sl.who); sl.who[n-1] != -1 || len(sl.more) != 7-(n-1) {
		t.Fatalf("640: who %v more %v", sl.who, sl.more)
	}
	first, end := sl.tiles[0], sl.tiles[len(sl.tiles)-1]
	if l, r := first.X, 640-end.Right(); l < tilePad || r < tilePad || l-r > 1 || r-l > 1 {
		t.Fatalf("640: row not centered: %v from the left, %v from the right", l, r)
	}
}

// fakeVideo is a remote video for tests.
type fakeVideo struct {
	id    string
	kind  rtc.VideoKind
	slot  video.Latest
	sizes [][2]int
}

func (f *fakeVideo) Identity() string    { return f.id }
func (f *fakeVideo) Kind() rtc.VideoKind { return f.kind }
func (f *fakeVideo) SetSize(w, h int)    { f.sizes = append(f.sizes, [2]int{w, h}) }
func (f *fakeVideo) Take(after uint64, fn func(video.Frame)) (uint64, bool) {
	return f.slot.Take(after, fn)
}

func TestTilePeople(t *testing.T) {
	st := callState{self: "me-0", talking: map[string]bool{"ann-1": true}, people: []rtc.Participant{
		{Identity: "ann-1", Name: "Ann Lee"}, {Identity: "wb", Name: "Whiteboard", Agent: true}, {Identity: "bob-2", Name: "Bob"},
	}}
	if ps := tilePeople(st, videoState{}, nil); ps != nil {
		t.Fatalf("no camera on anywhere, yet tiles: %v", ps)
	}
	bob := &fakeVideo{id: "bob-2", kind: rtc.Camera}
	share := &fakeVideo{id: "ann-1", kind: rtc.Screen}
	late := &fakeVideo{id: "cy-3", kind: rtc.Camera}
	ps := tilePeople(st, videoState{tracks: []rtc.VideoTrack{share, bob, late}}, map[string]bool{"bob-2": true})
	var names []string
	for _, p := range ps {
		names = append(names, p.name+"/"+p.initials)
	}
	if got := names; !slices.Equal(got, []string{"You/M", "Ann Lee/AL", "Bob/B", "cy/C"}) {
		t.Fatalf("people %v", got)
	}
	if !ps[0].self || ps[0].camOn || ps[1].camOn || !ps[1].talking || ps[2].track != bob || !ps[2].muted || ps[3].track != late {
		t.Fatalf("tiles %+v", ps)
	}
	if prio := tilePrio(ps); !slices.Equal(prio, []int{0, 2, 3, 1}) {
		t.Fatalf("prio %v: want you, the cameras, then the rest", prio)
	}
}

// A share that starts takes the stage; the board comes back when it ends;
// a new board version while a share is up badges the board's tab.
func TestStageFollowsShares(t *testing.T) {
	s := &meetingScene{}
	ann := shareTab{id: "ann-1"}
	bob := shareTab{id: "bob-2"}
	s.followShares([]shareTab{ann})
	if s.onStage != "ann-1" {
		t.Fatalf("a new share didn't take the stage: %q", s.onStage)
	}
	s.onStage = "" // back to the board by the tab
	s.followShares([]shareTab{ann})
	if s.onStage != "" {
		t.Fatalf("an old share took the stage back: %q", s.onStage)
	}
	s.followShares([]shareTab{ann, bob})
	if s.onStage != "bob-2" {
		t.Fatalf("the newest share should take the stage: %q", s.onStage)
	}
	s.st.seq, s.boardSeen = 5, 5
	if s.boardBadge() {
		t.Fatal("a badge with nothing new")
	}
	s.st.seq = 6
	if !s.boardBadge() {
		t.Fatal("no badge for a new version during a share")
	}
	s.followShares([]shareTab{ann})
	if s.onStage != "" {
		t.Fatalf("the share on stage ended, yet %q is on stage", s.onStage)
	}
	if s.boardBadge() {
		t.Fatal("a badge with the board on stage")
	}
}

// fakeSink stands in for the renderer.
type fakeSink struct {
	put     map[draw.ImageID]video.Frame
	dropped []draw.ImageID
}

func (f *fakeSink) PutFrame(id draw.ImageID, fr video.Frame, aspect float32) {
	if f.put == nil {
		f.put = map[draw.ImageID]video.Frame{}
	}
	f.put[id] = fr
}
func (f *fakeSink) DropFrame(id draw.ImageID) { f.dropped = append(f.dropped, id) }
func (f *fakeSink) VideoErr() error           { return nil }

// Each track hears its size when it changes, 0, 0 once when it isn't
// shown, and its frames go to the GPU only when new; a track gone lets its
// image go.
func TestSceneVideo(t *testing.T) {
	sink := &fakeSink{}
	sv := sceneVideo{frames: sink}
	a := &fakeVideo{id: "ann-1", kind: rtc.Camera}
	b := &fakeVideo{id: "bob-2", kind: rtc.Screen}
	f := video.NewFrame(64, 36)
	a.slot.Put(f)
	frame := func(shown bool, tracks ...rtc.VideoTrack) bool {
		sv.begin()
		has := false
		if shown {
			_, has = sv.take("ann-1/camera", a, tileAspect, false)
			sv.ask(a, "ann-1/camera", draw.Rect{W: 160, H: 90}, 2)
		}
		sv.end(tracks, false)
		return has
	}
	if !frame(true, a, b) {
		t.Fatal("a frame came but there's no picture")
	}
	frame(true, a, b)
	frame(true, a, b)
	if want := [][2]int{{320, 180}}; !slices.Equal(a.sizes, want) {
		t.Fatalf("ann told %v, want %v", a.sizes, want)
	}
	if want := [][2]int{{0, 0}}; !slices.Equal(b.sizes, want) {
		t.Fatalf("bob (not shown) told %v, want %v", b.sizes, want)
	}
	if len(sink.put) != 1 {
		t.Fatalf("%d images put", len(sink.put))
	}
	id := sv.shown["ann-1/camera"].id
	frame(false, a, b)
	if !slices.Equal(a.sizes, [][2]int{{320, 180}, {0, 0}}) {
		t.Fatalf("hidden: ann told %v", a.sizes)
	}
	frame(false, b)
	if !slices.Equal(sink.dropped, []draw.ImageID{id}) {
		t.Fatalf("ann's track went, dropped %v", sink.dropped)
	}
	// Your camera is mirrored whatever its frames say.
	var me video.Latest
	me.Put(f)
	sv.begin()
	sv.take(selfCamKey, &me, tileAspect, true)
	if !sink.put[sv.shown[selfCamKey].id].Mirror {
		t.Fatal("your camera wasn't mirrored")
	}
}

// The poller wakes the loop when a video on screen has a new frame, and
// not otherwise.
func TestFramePoll(t *testing.T) {
	var wakes atomic.Int32
	p := framePoll{wake: func() { wakes.Add(1) }}
	var l video.Latest
	p.set([]string{"a"}, []frameSource{&l})
	l.Put(video.NewFrame(4, 4))
	waitFor(t, "a wake", time.Second, func() bool { return wakes.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := wakes.Load(); n != 1 {
		t.Fatalf("woke %d times for one frame", n)
	}
	l.Put(video.NewFrame(4, 4))
	waitFor(t, "a second wake", time.Second, func() bool { return wakes.Load() == 2 })
	p.set(nil, nil)
	time.Sleep(20 * time.Millisecond)
	l.Put(video.NewFrame(4, 4))
	time.Sleep(20 * time.Millisecond)
	if n := wakes.Load(); n != 2 {
		t.Fatalf("woke for a video not on screen (%d)", n)
	}
}

// The Camera section lists the cameras with the pick checked, and says
// why when they can't be listed or the camera won't start.
func TestCameraRows(t *testing.T) {
	rows := cameraRows(renderCameras, nil, devicePrefs{Camera: "desk"}, videoState{}, "")
	var on []string
	for _, r := range rows {
		if r.kind == rowChoice && !r.camera {
			t.Fatalf("a camera row that picks something else: %q", r.label)
		}
		if r.on {
			on = append(on, r.label)
		}
		if r.kind == rowNote {
			t.Fatalf("a note with nothing wrong: %q", r.label)
		}
	}
	if !slices.Equal(on, []string{"Desk Camera"}) || rows[1].detail != "Built-in Camera" {
		t.Fatalf("checked %v; default's detail %q", on, rows[1].detail)
	}
	rows = cameraRows(nil, errors.New("video isn't built yet"), devicePrefs{}, videoState{}, "video isn't built yet")
	if len(rows) != 3 || rows[1].label != "Can't list the cameras: video isn't built yet" || rows[2].label != "Camera: video isn't built yet" {
		t.Fatalf("rows %+v", rows)
	}
	rows = cameraRows(renderCameras, nil, devicePrefs{Camera: "gone"}, videoState{}, "")
	if r := rows[len(rows)-1]; r.kind != rowNote || r.label != "The one picked isn't plugged in: using the default" {
		t.Fatalf("a camera picked that's gone: last row %+v", r)
	}
}
