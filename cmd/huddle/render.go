package main

// Offscreen rendering of the meeting window: the render command, and the
// way an agent looks at what it built.
//
// Copied from an earlier project of the author's (the offscreen device and
// text pipeline), drawing the meeting window.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/gfx"
	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
	"github.com/anthonyrego/huddle/internal/video"
	"github.com/anthonyrego/huddle/internal/wire"
)

func init() {
	register(&command{name: "render", summary: "draw the meeting window to a PNG (no window, no Dock icon)", run: runRender})
}

// offscreen is a device, renderer and text pipeline with no window.
type offscreen struct {
	device   *sdl.GPUDevice
	renderer *gfx.Renderer
	shaper   *text.Shaper
	glyphs   *text.GlyphCache
}

// newOffscreen initializes SDL in background mode (no Dock icon, no focus
// change) and creates the renderer. Must run on the main thread.
func newOffscreen(debug bool) (*offscreen, func(), error) {
	if err := platform.Init(platform.Background); err != nil {
		return nil, nil, err
	}
	device, err := gfx.NewDevice(debug)
	if err != nil {
		platform.Quit()
		return nil, nil, err
	}
	r, err := gfx.NewRenderer(device)
	if err != nil {
		device.Destroy()
		platform.Quit()
		return nil, nil, err
	}
	shaper, glyphs, err := newTextPipeline()
	if err != nil {
		r.Destroy()
		device.Destroy()
		platform.Quit()
		return nil, nil, err
	}
	r.SetGlyphCache(glyphs)
	o := &offscreen{device: device, renderer: r, shaper: shaper, glyphs: glyphs}
	return o, func() { r.Destroy(); device.Destroy(); platform.Quit() }, nil
}

// newTextPipeline returns the shaper and a glyph cache over the default
// rasterizer.
func newTextPipeline() (*text.Shaper, *text.GlyphCache, error) {
	// Load the system font index in the background, so the first line that
	// needs a fallback face doesn't wait for it.
	text.PreloadFonts()
	shaper, err := text.NewShaper()
	if err != nil {
		return nil, nil, err
	}
	rast, err := text.NewDefaultRasterizer()
	if err != nil {
		return nil, nil, err
	}
	return shaper, text.NewGlyphCache(rast), nil
}

// renderCamErr is the camera error a render shows unless -cam-err gives
// one: the longest the app says (no permission).
var renderCamErr = errors.New("Allow in System Settings › Privacy & Security › Camera")

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fixture := fs.String("fixture", "", "ops file to put on the board (one JSON op per line)")
	version := fs.String("version", "", "a board.version (JSON) to draw at the server's placement, as an app in the room would")
	typed := fs.String("typed", "", "text in the command field (focused)")
	addressing := fs.Bool("addressing", false, "draw the push-to-address key held")
	out := fs.String("out", "", "PNG to write (required)")
	width := fs.Float64("width", 1200, "window width, logical units")
	height := fs.Float64("height", 800, "window height, logical units")
	scale := fs.Float64("scale", 2, "pixels per logical unit")
	dark := fs.Bool("dark", false, "use the dark palette")
	room := fs.String("room", "design-review", "the room the bar names")
	identity := fs.String("identity", "", "who the bar says you are")
	conn := fs.String("conn", connConnected, "the connection state the pill shows (or unreachable, refused, retrying)")
	retryIn := fs.Int("retry-in", 0, "with a -conn that is down: seconds to the next try")
	server := fs.String("server", "", "the server host the bar names while the connection is down")
	agent := fs.String("agent", "", "the whiteboard's state (idle, listening, working, speaking, auto, offline)")
	detail := fs.String("agent-detail", "", "with -agent: the detail after it")
	queued := fs.Int("queued", 0, "with -agent: commands waiting")
	people := fs.String("people", "", "the voice session's people, comma-separated: a trailing * is talking, ! the whiteboard (and you first, as you)")
	mic := fs.String("mic", "", "with -people: the mic, on|muted|listening|lost|noecho (on without echo cancelling: it would not start)|waiting|retry|reconnecting")
	menu := fs.Bool("devices", false, "with -people: the device menu open, on made-up devices")
	echoOff := fs.Bool("echo-off", false, "with -devices: echo cancelling turned off in the menu")
	var vf videoFlags
	vf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	*people = vf.people(*people)
	if *out == "" {
		return errors.New("-out is required")
	}
	var ops []board.Op
	var place *layout.Placement
	seq := int64(0)
	if *fixture != "" {
		var err error
		if ops, err = board.ReadOps(*fixture); err != nil {
			return err
		}
		seq = int64(len(ops))
	}
	if *version != "" {
		b, err := os.ReadFile(*version)
		if err != nil {
			return err
		}
		var v wire.BoardVersion
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", *version, err)
		}
		snap, err := board.UnmarshalSnapshot(v.Doc)
		if err != nil {
			return err
		}
		ops, seq = snap.Ops, v.Seq
		if len(v.Layout) > 0 {
			place = new(layout.Placement)
			if err := json.Unmarshal(v.Layout, place); err != nil {
				return fmt.Errorf("%s: layout: %w", *version, err)
			}
		}
	}

	o, done, err := newOffscreen(false)
	if err != nil {
		return err
	}
	defer done()
	pal := theme.For(*dark)
	now := time.Now()
	sc := float32(*scale)
	s := newMeetingScene(o.shaper, func() float32 { return sc }, &meeting{})
	s.vid.frames = o.renderer
	switch *conn {
	case "unreachable":
		*conn = connUnreachable
	case "refused":
		*conn = connRefused
	case "retrying":
		*conn = connRetrying
	}
	st := meetingState{rev: 1, conn: *conn, room: *room, identity: *identity, seq: seq, ops: ops, place: place, server: *server, retryIn: *retryIn}
	if *typed != "" {
		s.field.SetText(*typed)
		s.field.Focus(true)
	}
	s.addressing = *addressing
	if *agent != "" {
		st.agent = wire.AgentStatus{State: wire.AgentState(*agent), Detail: *detail, Queued: *queued}
	}
	s.apply(st, now)
	var fakes []*fakeTrack
	if *people != "" {
		s.call = renderCall(*people, *mic)
		if fakes, err = vf.apply(s); err != nil {
			return err
		}
		if *menu {
			s.call.prefs = devicePrefs{Speakers: "monitor", EchoOff: *echoOff, Camera: vf.pickCam}
			var camErr error
			if vf.noCams {
				camErr = renderCamErr
			}
			s.openMenu(renderDevices, nil, renderCameras, camErr)
			s.menu.hover = 3
		}
	}
	w, h := float32(*width), float32(*height)
	var l draw.List
	s.draw(&l, w, h, pal, now) // lays the board out at now
	// Draw it settled: every glide and fade done.
	l.Reset()
	s.draw(&l, w, h, pal, now.Add(10*time.Second))

	pw, ph := int(w*sc+0.5), int(h*sc+0.5)
	b := gfx.Batch{Glyphs: o.glyphs}
	b.Reset(pw, ph)
	b.AddList(&l, sc)
	img, err := gfx.RenderImage(o.device, o.renderer, pw, ph, pal.Background, &b)
	if err != nil {
		return err
	}
	if err := writePNG(*out, img); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d ops, %d draw ops)\n", *out, len(ops), len(l.Ops()))
	for _, t := range fakes {
		fmt.Fprintf(os.Stderr, "  %s %s: SetSize %dx%d\n", t.id, t.kind, t.w, t.h)
	}
	return nil
}

// videoFlags are render's made-up video: who has a camera on or off, who
// shares, what's on the stage, and the buttons' states.
type videoFlags struct {
	cams, camOff, share, muted string
	stage, camera, sharing     string
	camErr, pickCam            string
	badge, noCams              bool
}

func (vf *videoFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&vf.cams, "cams", "", "people with a camera on, comma-separated: a trailing * is talking; You is you (mirrored); others not in -people join it")
	fs.StringVar(&vf.camOff, "cam-off", "", "people in the call with their camera off, comma-separated (their tiles show initials)")
	fs.StringVar(&vf.share, "share", "", "people sharing a screen, comma-separated")
	fs.StringVar(&vf.stage, "stage", "", "with -share: what's on the stage, board or share (default: the share, as when it starts)")
	fs.BoolVar(&vf.badge, "badge", false, "with a share on the stage: the board changed since it was last on stage")
	fs.StringVar(&vf.muted, "muted", "", "people whose tiles show the muted mic, comma-separated (you: -mic muted)")
	fs.StringVar(&vf.camera, "camera", "", "the Camera button: on|off|busy|error (default: on when -cams has You)")
	fs.StringVar(&vf.sharing, "sharing", "", "the Share button: on|off|busy|error")
	fs.StringVar(&vf.camErr, "cam-err", "", "with -camera error: the call's reason (default: the stub's error, as the button hears it)")
	fs.StringVar(&vf.pickCam, "pick-camera", "desk", "with -devices: the camera picked (a UID of the made-up cameras; empty is the default)")
	fs.BoolVar(&vf.noCams, "no-cameras", false, "with -devices: the cameras can't be listed (the stub's error)")
}

// splitNames splits a comma-separated list, trimmed, dropping empties.
func splitNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// people is -people with everyone the video flags name added: you first,
// then the rest in the order they're named.
func (vf *videoFlags) people(people string) string {
	names := splitNames(people)
	if len(names) == 0 && (vf.cams != "" || vf.camOff != "" || vf.share != "" || vf.camera != "" || vf.sharing != "") {
		names = []string{"You"}
	}
	have := map[string]bool{}
	for _, n := range names {
		have[strings.TrimRight(n, "*!")] = true
	}
	have["You"] = true
	for _, list := range []string{vf.cams, vf.camOff, vf.share} {
		for _, n := range splitNames(list) {
			if base := strings.TrimRight(n, "*!"); !have[base] {
				have[base] = true
				names = append(names, n)
			}
		}
	}
	return strings.Join(names, ",")
}

// apply gives s's call the made-up video: a pattern camera for everyone in
// -cams (each at a frame number of their own, so they look different;
// yours mirrored), a pattern screen for everyone in -share, and the stage
// and the buttons as asked. It returns the tracks, to report the sizes
// they were told.
func (vf *videoFlags) apply(s *meetingScene) ([]*fakeTrack, error) {
	c := s.call
	st := &c.st
	ids := map[string]string{"You": st.self}
	for _, p := range st.people {
		ids[p.Name] = p.Identity
	}
	id := func(n string) (string, error) {
		n = strings.TrimRight(n, "*!")
		if i, ok := ids[n]; ok {
			return i, nil
		}
		return "", fmt.Errorf("%q isn't in the call", n)
	}
	var fakes []*fakeTrack
	v := &c.vid
	for i, n := range splitNames(vf.cams) {
		who, err := id(n)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(n, "*") {
			st.talking[who] = true
		}
		if who == st.self {
			f := video.NewFrame(640, 360)
			video.Draw(&f, 1234, false)
			f.Mirror = true
			v.self, v.camera = &video.Latest{}, true
			v.self.Put(f)
			continue
		}
		t := newFakeTrack(who, rtc.Camera, 640, 360, uint32(1000*(i+1)+i*97))
		fakes = append(fakes, t)
		v.tracks = append(v.tracks, t)
	}
	for _, n := range splitNames(vf.camOff) {
		if _, err := id(n); err != nil {
			return nil, err
		}
	}
	for i, n := range splitNames(vf.share) {
		who, err := id(n)
		if err != nil {
			return nil, err
		}
		t := newFakeTrack(who, rtc.Screen, 1440, 900, uint32(31337+i*500))
		fakes = append(fakes, t)
		v.tracks = append(v.tracks, t)
	}
	if vf.muted != "" {
		s.mutedTiles = map[string]bool{}
		for _, n := range splitNames(vf.muted) {
			who, err := id(n)
			if err != nil {
				return nil, err
			}
			s.mutedTiles[who] = true
		}
	}
	// What the stage shows: the newest share takes it, as when it starts,
	// unless -stage board says the board's back.
	tabs := screenTabs(c.snapshot(), *v)
	s.followShares(tabs)
	switch vf.stage {
	case "", "share":
	case "board":
		s.onStage = ""
	default:
		return nil, fmt.Errorf("-stage %q: want board or share", vf.stage)
	}
	if vf.badge {
		s.boardSeen = s.st.seq - 1
	}
	errText := renderCamErr.Error()
	switch vf.camera {
	case "":
	case "on":
		v.camera = true
	case "off":
		v.camera, v.self = false, nil
	case "busy":
		s.asks.camBusy = true
	case "error":
		v.camera, v.self = false, nil
		if vf.camErr != "" {
			v.camErr = vf.camErr
		} else {
			s.asks.camErr = errText
		}
	default:
		return nil, fmt.Errorf("-camera %q: want on, off, busy or error", vf.camera)
	}
	switch vf.sharing {
	case "", "off":
	case "on":
		v.sharing = true
	case "busy":
		s.asks.shareBusy = true
	case "error":
		s.asks.shareErr = errText
	default:
		return nil, fmt.Errorf("-sharing %q: want on, off, busy or error", vf.sharing)
	}
	return fakes, nil
}

// fakeTrack is a remote video for render: one pattern frame, and the
// size it was last told.
type fakeTrack struct {
	id   string
	kind rtc.VideoKind
	slot video.Latest
	w, h int
}

func newFakeTrack(id string, kind rtc.VideoKind, w, h int, n uint32) *fakeTrack {
	t := &fakeTrack{id: id, kind: kind, w: -1, h: -1}
	f := video.NewFrame(w, h)
	video.Draw(&f, n, false)
	t.slot.Put(f)
	return t
}

func (t *fakeTrack) Identity() string    { return t.id }
func (t *fakeTrack) Kind() rtc.VideoKind { return t.kind }
func (t *fakeTrack) SetSize(w, h int)    { t.w, t.h = w, h }
func (t *fakeTrack) Take(after uint64, fn func(video.Frame)) (uint64, bool) {
	return t.slot.Take(after, fn)
}

// renderCameras are made-up cameras for render -devices.
var renderCameras = []cameraDevice{
	{UID: "built-in", Name: "Built-in Camera", Transport: "built-in", Default: true},
	{UID: "desk", Name: "Desk Camera", Transport: "USB"},
	{UID: "phone", Name: "Phone Camera", Transport: "Continuity"},
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
