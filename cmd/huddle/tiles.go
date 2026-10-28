package main

// Camera tiles. Once anyone in the call has a camera on, tiles run down
// the right of the window, or along the top of the stage when the window
// is too narrow for both: you first, then everyone else in the order they
// came, never the whiteboard. A tile is 16:9 and shows the person's camera
// (cropped to fill it), or their initials while it's off; their name (you
// are "You", and your camera is mirrored, as a mirror shows you); a ring
// while they talk; and a crossed-out mic when they're muted. When the
// tiles don't all fit, the last one says how many more there are, keeping
// you and the cameras that are on before the cameras that are off. With no
// camera on anywhere there are no tiles: the bar's chips say who's here,
// and the board keeps the window.
//
// Video reaches the GPU from here (sceneVideo): each frame, every video on
// screen is taken from its source, the newest frame if it's newer than
// the last taken, and put to the renderer, which converts it before the
// frame draws (gfx.Renderer.PutFrame). Each track is told the size it's
// shown at, in pixels, so the room can send the layer that fits, and 0, 0
// once it isn't shown. A poller (framePoll) wakes the loop when a video on
// screen has a new frame.

import (
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
	"github.com/anthonyrego/huddle/internal/video"
)

// Tile layout, logical units.
const (
	tilePad    = 12  // around the tiles, inside their strip
	tileGap    = 8   // between tiles
	tileMaxW   = 248 // a tile's width in the column, at most
	tileMinW   = 168 // and at least, unless there are too many to fit
	tileFloorW = 120 // the smallest a column tile shrinks to
	rowTileMin = 54  // a row tile's height, at least and at most
	rowTileMax = 84
	stageMinW  = 560 // the stage keeps this wide beside the column; narrower, the tiles fold into a row
	tileRadius = 10
	tileAspect = float32(16) / 9
)

// frameSource is a video the scene takes frames from: a remote track, or
// your camera's slot.
type frameSource interface {
	Take(after uint64, fn func(video.Frame)) (uint64, bool)
}

// frameSink takes frames to the GPU: the renderer.
type frameSink interface {
	PutFrame(id draw.ImageID, f video.Frame, aspect float32)
	DropFrame(id draw.ImageID)
	VideoErr() error
}

// shownVideo is what the scene keeps of a video it shows, or has.
type shownVideo struct {
	id     draw.ImageID
	seq    uint64 // the last frame taken
	fw, fh int    // that frame's size (0 before the first)
	asked  [2]int // what SetSize was last told; -1 before anything
	used   bool   // on screen this frame
}

// sceneVideo is the scene's video: the frames it has taken, the image
// each draws, and what each track was told. Main thread only.
type sceneVideo struct {
	frames  frameSink // nil: no GPU (the tiles show initials)
	shown   map[string]*shownVideo
	nextID  draw.ImageID
	poll    framePoll
	keys    []string // on screen this frame, for the poller
	srcs    []frameSource
	lastErr string
}

// imageBase keeps video image ids clear of any other image's.
const imageBase = draw.ImageID(1) << 40

func (sv *sceneVideo) get(key string) *shownVideo {
	if sv.shown == nil {
		sv.shown = map[string]*shownVideo{}
	}
	v := sv.shown[key]
	if v == nil {
		sv.nextID++
		v = &shownVideo{id: imageBase + sv.nextID, asked: [2]int{-1, -1}}
		sv.shown[key] = v
	}
	return v
}

// begin starts a frame: nothing is on screen yet.
func (sv *sceneVideo) begin() {
	for _, v := range sv.shown {
		v.used = false
	}
	sv.keys, sv.srcs = sv.keys[:0], sv.srcs[:0]
}

// take shows the video key from src this frame: a frame newer than the
// last one taken goes to the GPU, cropped to aspect (0: whole) and
// mirrored if it says so or mirror does (your own camera, whoever made
// the frame). It reports whether there's a picture to draw.
func (sv *sceneVideo) take(key string, src frameSource, aspect float32, mirror bool) (*shownVideo, bool) {
	v := sv.get(key)
	v.used = true
	if sv.frames == nil || src == nil {
		return v, false
	}
	sv.keys, sv.srcs = append(sv.keys, key), append(sv.srcs, src)
	if seq, ok := src.Take(v.seq, func(f video.Frame) {
		f.Mirror = f.Mirror || mirror
		sv.frames.PutFrame(v.id, f, aspect)
		v.fw, v.fh = f.W, f.H
	}); ok {
		v.seq = seq
	}
	return v, v.fw > 0
}

// ask tells t the size it's shown at, r in logical units at scale, when
// that changed.
func (sv *sceneVideo) ask(t rtc.VideoTrack, key string, r draw.Rect, scale float32) {
	sv.setSize(t, sv.get(key), int(r.W*scale+0.5), int(r.H*scale+0.5))
}

func (sv *sceneVideo) setSize(t rtc.VideoTrack, v *shownVideo, w, h int) {
	if v.asked == [2]int{w, h} {
		return
	}
	v.asked = [2]int{w, h}
	t.SetSize(w, h)
}

// end finishes a frame: tracks not on screen are told 0, 0; the videos of
// tracks that have gone (and your camera, once it's off) are let go; the
// poller watches what's on screen.
func (sv *sceneVideo) end(tracks []rtc.VideoTrack, self bool) {
	live := map[string]bool{}
	if self {
		live[selfCamKey] = true
	}
	for _, t := range tracks {
		key := videoKey(t)
		live[key] = true
		if v := sv.get(key); !v.used {
			sv.setSize(t, v, 0, 0)
		}
	}
	for key, v := range sv.shown {
		if !live[key] {
			if sv.frames != nil {
				sv.frames.DropFrame(v.id)
			}
			delete(sv.shown, key)
		}
	}
	sv.poll.set(sv.keys, sv.srcs)
	if sv.frames == nil {
		return
	}
	if err := sv.frames.VideoErr(); err != nil && err.Error() != sv.lastErr {
		slog.Warn("video: frames not converted", "err", err)
		sv.lastErr = err.Error()
	} else if err == nil {
		sv.lastErr = ""
	}
}

const selfCamKey = "you/camera"

func videoKey(t rtc.VideoTrack) string { return t.Identity() + "/" + t.Kind().String() }

// framePoll wakes the loop when a video on screen has a new frame. Its
// goroutine looks every few milliseconds while any is on screen (a Take
// with nothing to do is a lock and a compare) and waits otherwise; the
// main thread does the copying when it draws.
type framePoll struct {
	wake func() // nil: no poller (render)

	mu      sync.Mutex
	keys    []string
	srcs    []frameSource
	kick    chan struct{}
	started bool
}

const pollEvery = 4 * time.Millisecond

// set is what's on screen now; main thread.
func (p *framePoll) set(keys []string, srcs []frameSource) {
	if p.wake == nil {
		return
	}
	p.mu.Lock()
	changed := !slices.Equal(keys, p.keys)
	if changed {
		p.keys, p.srcs = slices.Clone(keys), slices.Clone(srcs)
	}
	p.mu.Unlock()
	if !p.started {
		p.started = true
		p.kick = make(chan struct{}, 1)
		go p.run()
	}
	if changed {
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
}

func (p *framePoll) run() {
	seen := map[string]uint64{}
	t := time.NewTimer(pollEvery)
	for {
		p.mu.Lock()
		keys, srcs := p.keys, p.srcs
		p.mu.Unlock()
		if len(keys) == 0 {
			<-p.kick
			continue
		}
		t.Reset(pollEvery)
		select {
		case <-p.kick:
			continue
		case <-t.C:
		}
		fresh := false
		for i, src := range srcs {
			if seq, ok := src.Take(seen[keys[i]], func(video.Frame) {}); ok {
				seen[keys[i]] = seq
				fresh = true
			}
		}
		if fresh {
			p.wake()
		}
	}
}

// tilePerson is someone with a tile.
type tilePerson struct {
	key      string // their identity
	name     string // "You" for you
	initials string
	self     bool
	talking  bool
	muted    bool           // known for you; the room doesn't say for others yet
	cam      frameSource    // their camera (nil: off)
	camOn    bool           // their camera is on (it may have no frame yet)
	track    rtc.VideoTrack // their camera's track (nil for you)
}

// tilePeople is who gets a tile: nobody while no camera is on anywhere,
// else you and everyone but the whiteboard. mutedSet marks others muted
// (render only, until the room says).
func tilePeople(st callState, v videoState, mutedSet map[string]bool) []tilePerson {
	cams := map[string]rtc.VideoTrack{}
	var order []string
	for _, t := range v.tracks {
		if t.Kind() == rtc.Camera {
			if _, dup := cams[t.Identity()]; !dup {
				order = append(order, t.Identity())
			}
			cams[t.Identity()] = t
		}
	}
	if !v.camera && len(cams) == 0 {
		return nil
	}
	var ps []tilePerson
	if st.self != "" {
		p := tilePerson{key: st.self, name: "You", self: true, camOn: v.camera, muted: st.muted,
			initials: initials(displayName(rtc.Participant{Identity: st.self})),
			talking:  st.talking[st.self] && !st.muted}
		if v.camera && v.self != nil {
			p.cam = v.self
		}
		ps = append(ps, p)
	}
	seen := map[string]bool{st.self: true}
	add := func(part rtc.Participant) {
		if part.Agent || seen[part.Identity] {
			return
		}
		seen[part.Identity] = true
		name := displayName(part)
		p := tilePerson{key: part.Identity, name: name, initials: initials(name), talking: st.talking[part.Identity], muted: mutedSet[part.Identity]}
		if t := cams[part.Identity]; t != nil {
			p.cam, p.camOn, p.track = t, true, t
		}
		ps = append(ps, p)
	}
	for _, part := range st.people {
		add(part)
	}
	for _, id := range order { // a camera from someone the call hasn't listed yet
		add(rtc.Participant{Identity: id})
	}
	return ps
}

// initials are the first letters of a name's first and last words.
func initials(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' })
	if len(words) == 0 {
		return "?"
	}
	first := func(w string) string {
		r, _ := utf8.DecodeRuneInString(w)
		return string(unicode.ToUpper(r))
	}
	s := first(words[0])
	if len(words) > 1 {
		s += first(words[len(words)-1])
	}
	return s
}

// stageLayout is where the stage and the tiles go in the window above
// the bar.
type stageLayout struct {
	stage draw.Rect   // the board or a share
	strip draw.Rect   // the tiles' column or row (empty: no tiles)
	row   bool        // the tiles run along the top
	tiles []draw.Rect // each tile shown
	who   []int       // the person in each tile (-1: the "+N" tile)
	more  []int       // the people the "+N" tile stands for
}

// layoutStage lays out a w×h area (the window above the bar) for n
// tiles. prio orders the people by who keeps a tile when they don't all
// fit, most wanted first.
func layoutStage(w, h float32, n int, prio []int) stageLayout {
	sl := stageLayout{stage: draw.Rect{W: w, H: h}}
	if n == 0 {
		return sl
	}
	tw := float32(math.Floor(float64(min(max(w*0.18, tileMinW), tileMaxW))))
	if w-(tw+2*tilePad) >= stageMinW {
		// A column down the right.
		avail := h - 2*tilePad
		th := tw / tileAspect
		k := n
		if fn := float32(n); fn*th+(fn-1)*tileGap > avail {
			tw = float32(math.Floor(float64((avail - (fn-1)*tileGap) / fn * tileAspect)))
			th = tw / tileAspect
			if tw < tileFloorW {
				tw = tileFloorW
				th = tw / tileAspect
				k = max(int((avail+tileGap)/(th+tileGap)), 1)
			}
		}
		sl.strip = draw.Rect{X: w - tw - 2*tilePad, W: tw + 2*tilePad, H: h}
		sl.stage.W = sl.strip.X
		sl.pick(n, k, prio)
		for i := range sl.who {
			sl.tiles = append(sl.tiles, draw.Rect{X: sl.strip.X + tilePad, Y: tilePad + float32(i)*(th+tileGap), W: tw, H: th})
		}
		return sl
	}
	// A row along the top, centered.
	tw = float32(math.Floor(float64(min(max(h*0.2, rowTileMin), rowTileMax) * tileAspect)))
	th := tw / tileAspect
	k := max(int((w-2*tilePad+tileGap)/(tw+tileGap)), 1)
	sl.row = true
	sl.strip = draw.Rect{W: w, H: th + 2*tilePad}
	sl.stage = draw.Rect{Y: sl.strip.H, W: w, H: h - sl.strip.H}
	sl.pick(n, k, prio)
	m := float32(len(sl.who))
	x := float32(math.Floor(float64(w-(m*tw+(m-1)*tileGap)) / 2))
	for i := range sl.who {
		sl.tiles = append(sl.tiles, draw.Rect{X: x + float32(i)*(tw+tileGap), Y: tilePad, W: tw, H: th})
	}
	return sl
}

// pick chooses who gets the k tiles there's room for: everyone if n fit,
// else the first k-1 by prio, in their own order, and a "+N" tile last.
func (sl *stageLayout) pick(n, k int, prio []int) {
	if n <= k {
		for i := range n {
			sl.who = append(sl.who, i)
		}
		return
	}
	keep := map[int]bool{}
	for _, i := range prio[:k-1] {
		keep[i] = true
	}
	for i := range n {
		if keep[i] {
			sl.who = append(sl.who, i)
		} else {
			sl.more = append(sl.more, i)
		}
	}
	sl.who = append(sl.who, -1)
}

// tilePrio orders people by who keeps a tile: you, then cameras on, then
// the rest, each in the call's order.
func tilePrio(ps []tilePerson) []int {
	prio := make([]int, len(ps))
	for i := range prio {
		prio[i] = i
	}
	rank := func(p tilePerson) int {
		switch {
		case p.self:
			return 0
		case p.camOn:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(prio, func(a, b int) int { return rank(ps[a]) - rank(ps[b]) })
	return prio
}

// drawTiles draws the strip and its tiles.
func (s *meetingScene) drawTiles(c *ui.Ctx, l *draw.List, sl stageLayout, ps []tilePerson) {
	if sl.strip.Empty() {
		return
	}
	pal := c.Pal
	l.Rect(sl.strip, pal.Bar, draw.Radii{})
	if sl.row {
		l.Rect(draw.Rect{X: sl.strip.X, Y: sl.strip.Bottom() - 1, W: sl.strip.W, H: 1}, pal.Border, draw.Radii{})
	} else {
		l.Rect(draw.Rect{X: sl.strip.X, Y: sl.strip.Y, W: 1, H: sl.strip.H}, pal.Border, draw.Radii{})
	}
	for i, r := range sl.tiles {
		if k := sl.who[i]; k >= 0 {
			s.drawTile(c, l, r, ps[k])
			continue
		}
		var names []string
		talking := false
		for _, k := range sl.more {
			names = append(names, ps[k].name)
			talking = talking || ps[k].talking
		}
		s.drawMoreTile(c, l, r, names, talking)
	}
}

// drawTile draws one person's tile in r.
func (s *meetingScene) drawTile(c *ui.Ctx, l *draw.List, r draw.Rect, p tilePerson) {
	pal := c.Pal
	rad := draw.Uniform(tileRadius)
	pic, has := (*shownVideo)(nil), false
	if p.cam != nil {
		key := selfCamKey
		if p.track != nil {
			key = videoKey(p.track)
		}
		pic, has = s.vid.take(key, p.cam, tileAspect, p.self)
		if p.track != nil {
			s.vid.ask(p.track, key, r, c.Scale)
		}
	}
	if has {
		l.Image(r, pic.id, rad, 255)
	} else {
		l.BorderedRect(r, pal.SurfaceAlt, pal.Border, 1, rad)
		d := snapHalf(min(r.H*0.46, 64))
		cy := r.Y + (r.H-d)/2 - min(8, r.H*0.06)
		circle := draw.Rect{X: r.X + (r.W-d)/2, Y: cy, W: d, H: d}
		l.BorderedRect(circle, pal.Surface, pal.Border, 1, draw.Uniform(d/2))
		lay := s.tc.lay(c, p.initials, text.SansSemibold, snapHalf(d*0.36))
		drawAt(l, lay, circle.X+(d-lay.Width)/2, circle.Y+(d-lay.Height)/2, pal.TextMuted)
	}
	s.drawTileLabel(c, l, r, p.name, p.muted)
	if p.talking {
		talkRing(l, r, pal.Accent)
	}
}

// drawTileLabel draws the name, and the muted mic before it, in a pill
// at the tile's bottom left.
func (s *meetingScene) drawTileLabel(c *ui.Ctx, l *draw.List, r draw.Rect, name string, muted bool) {
	pal := c.Pal
	size := float32(11.5)
	if r.H < 90 {
		size = 10.5
	}
	lay := s.tc.lay(c, name, text.SansSemibold, size)
	icon := float32(0)
	if muted {
		icon = 13
	}
	inset := float32(6)
	if r.H < 90 {
		inset = 4
	}
	maxW := r.W - 2*inset - 16 - icon
	h := lay.Height + 7
	pill := draw.Rect{X: r.X + inset, Y: r.Bottom() - inset - h, W: min(lay.Width, maxW) + 16 + icon, H: h}
	l.Rect(pill, pal.Surface.WithAlpha(0xe6), draw.Uniform(h/2))
	if muted {
		micOff(l, pill.X+12, pill.Y+h/2, pal.Error)
	}
	putFitAt(c, &s.tc, l, lay, pill.X+8+icon, pill.Y+(h-lay.Height)/2, maxW, pal.Text, text.SansSemibold, size)
}

// drawMoreTile is the last tile when they don't all fit: how many more,
// and who.
func (s *meetingScene) drawMoreTile(c *ui.Ctx, l *draw.List, r draw.Rect, names []string, talking bool) {
	pal := c.Pal
	l.BorderedRect(r, pal.SurfaceAlt, pal.Border, 1, draw.Uniform(tileRadius))
	big := s.tc.lay(c, "+"+itoa(len(names)), text.SansSemibold, snapHalf(min(r.H*0.24, 22)))
	small := s.tc.lay(c, strings.Join(names, ", "), text.SansRegular, 11)
	gap := float32(4)
	top := r.Y + (r.H-big.Height-gap-small.Height)/2
	drawAt(l, big, r.X+(r.W-big.Width)/2, top, pal.TextMuted)
	w := min(small.Width, r.W-16)
	putFitAt(c, &s.tc, l, small, r.X+(r.W-w)/2, top+big.Height+gap, r.W-16, pal.TextFaint, text.SansRegular, 11)
	if talking {
		talkRing(l, r, pal.Accent)
	}
}

// talkRing is the ring around a tile while its person talks.
func talkRing(l *draw.List, r draw.Rect, col draw.Color) {
	const out = 3
	l.BorderedRect(draw.Rect{X: r.X - out, Y: r.Y - out, W: r.W + 2*out, H: r.H + 2*out}, col.WithAlpha(0), col, 2, draw.Uniform(tileRadius+out))
}

// micOff draws a small crossed-out mic centered at x, y.
func micOff(l *draw.List, x, y float32, col draw.Color) {
	l.BorderedRect(draw.Rect{X: x - 2, Y: y - 5, W: 4, H: 6.5}, col.WithAlpha(0), col, 1.2, draw.Uniform(2))
	var arc []draw.Point
	for i := range 7 {
		a := math.Pi * float64(i) / 6
		arc = append(arc, draw.Point{X: x - 3.5*float32(math.Cos(a)), Y: y - 0.5 + 3*float32(math.Sin(a))})
	}
	l.Path(draw.Path{Points: arc, Stroke: col, Width: 1.1})
	l.Path(draw.Path{Points: []draw.Point{{X: x, Y: y + 2.5}, {X: x, Y: y + 4.5}}, Stroke: col, Width: 1.1})
	l.Path(draw.Path{Points: []draw.Point{{X: x - 4.5, Y: y - 5}, {X: x + 4.5, Y: y + 4.5}}, Stroke: col, Width: 1.3})
}

// snapHalf rounds a text size to half a unit, so sizes that follow a
// tile's don't each shape and rasterize anew.
func snapHalf(v float32) float32 { return float32(math.Round(float64(v)*2)) / 2 }
