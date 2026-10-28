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
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/gfx"
	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
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
	conn := fs.String("conn", connConnected, "the connection state the pill shows")
	agent := fs.String("agent", "", "the whiteboard's state (idle, listening, working, speaking, auto, offline)")
	detail := fs.String("agent-detail", "", "with -agent: the detail after it")
	queued := fs.Int("queued", 0, "with -agent: commands waiting")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	st := meetingState{rev: 1, conn: *conn, room: *room, identity: *identity, seq: seq, ops: ops, place: place}
	if *typed != "" {
		s.field.SetText(*typed)
		s.field.Focus(true)
	}
	s.addressing = *addressing
	if *agent != "" {
		st.agent = wire.AgentStatus{State: wire.AgentState(*agent), Detail: *detail, Queued: *queued}
	}
	s.apply(st, now)
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
	return nil
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
