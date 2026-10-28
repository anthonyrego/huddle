package main

// The window: SDL, the GPU device, the text pipeline and the event loop.
// The loop sleeps in WaitEvent while nothing changes, so an idle window
// renders no frames. The connection runs on its own goroutines and wakes
// the loop when the meeting changes.
//
// Copied from an earlier project of the author's (without the overlay,
// the history and the saved window state).

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/gfx"
	"github.com/anthonyrego/huddle/internal/logx"
	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
)

func init() {
	register(&command{name: "app", summary: "the meeting window: join a room and see its board", run: runApp})
}

const (
	themeLight = "light"
	themeDark  = "dark"
)

// winApp is a window, its renderer and the text pipeline, drawing a scene.
// All of its methods run on the main thread.
type winApp struct {
	device   *sdl.GPUDevice
	window   *gfx.Window
	renderer *gfx.Renderer
	glyphs   *text.GlyphCache
	shaper   *text.Shaper
	list     draw.List
	batch    gfx.Batch
	pal      theme.Palette
	// theme is the theme asked for (see dark): "" follows the system.
	theme string
	scene *meetingScene
	// scale is the display scale glyphs were last rasterized for.
	scale   float32
	stalled bool // the last frame got no swapchain texture (minimized or covered)

	frames  atomic.Int64
	cleanup []func()
}

func runApp(args []string) error {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	server := fs.String("server", envOr("HUDDLE_SERVER", "http://localhost:8080"), "the huddle server (HUDDLE_SERVER)")
	room := fs.String("room", "", "join this room (dev)")
	name := fs.String("name", defaultName(), "your name in the room")
	fixture := fs.String("fixture", "", "dev: replay this ops file on the board instead of joining (no server)")
	every := fs.Duration("every", 500*time.Millisecond, "with -fixture: time between ops")
	debug := fs.Bool("debug", false, "enable GPU validation")
	exitAfter := fs.Duration("exit-after", 0, "quit by itself after this long (unattended runs)")
	size := fs.String("size", "1200x800", "window size in points, WxH")
	themeMode := fs.String("theme", "", "the window's theme, light|dark (default: the system's)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	switch {
	case *fixture != "":
	case *room != "":
		if err := writeLastJoin(paths.Data, lastJoin{Server: *server, Room: *room, Name: *name}); err != nil {
			return err
		}
	default:
		if j, ok := readLastJoin(paths.Data); ok {
			*server, *room = j.Server, j.Room
			if !flagSet(fs, "name") && j.Name != "" {
				*name = j.Name
			}
		}
	}
	logf, err := logx.Setup(paths.Logs, "app", false)
	if err != nil {
		return err
	}
	defer logf.Close()

	var ops []board.Op
	if *fixture != "" {
		if ops, err = board.ReadOps(*fixture); err != nil {
			return err
		}
	}
	cfg := gfx.WindowConfig{Title: "Huddle", Hidden: true}
	if _, err := fmt.Sscanf(strings.ToLower(*size), "%dx%d", &cfg.Width, &cfg.Height); err != nil {
		return fmt.Errorf("-size %q: want WxH", *size)
	}
	a, err := openWindow(cfg, *debug)
	if err != nil {
		return err
	}
	defer a.close()
	a.window.Handle().SetMinimumSize(640, 420)
	switch *themeMode {
	case "", themeLight, themeDark:
		a.theme = *themeMode
	default:
		return fmt.Errorf("-theme %q: want light or dark", *themeMode)
	}
	a.pal = theme.For(a.dark())

	mt := &meeting{wake: func() { platform.Wake() }}
	a.scene = newMeetingScene(a.shaper, func() float32 { return a.window.Scale() }, mt)
	a.scene.translate = a.translate

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	switch {
	case *fixture != "":
		mt.update(func(s *meetingState) { s.conn, s.room = connFixture, *fixture })
		go func() { defer close(done); playFixture(ctx, ops, *every, mt) }()
	case *room == "":
		mt.update(func(s *meetingState) {
			s.conn, s.detail = connNoRoom, "open Huddle.app --args app -server URL -room NAME"
		})
		close(done)
	default:
		c := newConn(*server, *room, *name, mt)
		a.scene.send = c.send
		a.scene.textInput = func(on bool) {
			if on {
				a.window.Handle().StartTextInput()
			} else {
				a.window.Handle().StopTextInput()
			}
		}
		go func() { defer close(done); c.run(ctx) }()
	}
	a.window.Handle().Show()
	err = a.run(*exitAfter)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("app: the connection didn't end in 2s")
	}
	return err
}

// playFixture shows ops one every interval, as versions would arrive,
// checking each against the board first.
func playFixture(ctx context.Context, ops []board.Op, every time.Duration, mt *meeting) {
	doc := board.New()
	var shown []board.Op
	t := time.NewTicker(every)
	defer t.Stop()
	for _, op := range ops {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := doc.Apply(op); err != nil {
				slog.Warn("fixture: op rejected", "op", op.Name, "id", op.ID, "err", err)
				continue
			}
			shown = append(shown, op)
			mt.setVersion(int64(len(shown)), append([]board.Op(nil), shown...), nil, []board.Op{op})
		}
	}
	mt.setConn(connFixtureEnd, "")
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// defaultName is the account's full name, else its login.
func defaultName() string {
	u, err := user.Current()
	if err != nil {
		return "Guest"
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Username
}

// openWindow sets up SDL, the GPU device, a window and the text pipeline.
func openWindow(cfg gfx.WindowConfig, debug bool) (*winApp, error) {
	var err error
	if err := platform.Init(platform.Foreground); err != nil {
		return nil, err
	}
	a := &winApp{pal: theme.For(sdl.GetSystemTheme() == sdl.SYSTEM_THEME_DARK)}
	a.cleanup = append(a.cleanup, platform.Quit)
	platform.QuitOnSignal()

	fail := func(err error) (*winApp, error) { a.close(); return nil, err }
	if a.device, err = gfx.NewDevice(debug); err != nil {
		return fail(err)
	}
	a.cleanup = append(a.cleanup, a.device.Destroy)
	if a.window, err = gfx.NewWindow(a.device, cfg); err != nil {
		return fail(err)
	}
	a.cleanup = append(a.cleanup, a.window.Destroy)
	if a.renderer, err = gfx.NewRenderer(a.device); err != nil {
		return fail(err)
	}
	a.cleanup = append(a.cleanup, a.renderer.Destroy)
	if a.shaper, a.glyphs, err = newTextPipeline(); err != nil {
		return fail(err)
	}
	a.renderer.SetGlyphCache(a.glyphs)
	a.batch.Glyphs = a.glyphs
	a.scale = a.window.Scale()
	slog.Info("gpu", "driver", a.device.Driver(), "sdl", sdl.GetVersion().String(), "scale", a.window.Scale(), "density", a.window.PixelDensity())
	return a, nil
}

func (a *winApp) win() *gfx.Window { return a.window }

func (a *winApp) close() {
	for i := len(a.cleanup) - 1; i >= 0; i-- {
		a.cleanup[i]()
	}
	a.cleanup = nil
}

// dark reports whether the window draws dark: the theme it was asked for,
// else the system's.
func (a *winApp) dark() bool {
	switch a.theme {
	case themeLight:
		return false
	case themeDark:
		return true
	}
	return sdl.GetSystemTheme() == sdl.SYSTEM_THEME_DARK
}

// run is the event loop. It sleeps in WaitEvent while nothing changes, so an
// idle window renders no frames; while the scene animates it renders
// continuously, paced by the swapchain (vsync).
func (a *winApp) run(exitAfter time.Duration) error {
	start := time.Now()
	if exitAfter > 0 {
		time.AfterFunc(exitAfter, func() { sdl.PushEvent(&sdl.Event{Type: sdl.EVENT_QUIT}) })
	}
	quit := func() error {
		slog.Info("quit", "frames", a.frames.Load(), "uptime", time.Since(start).Round(time.Millisecond))
		return nil
	}

	// macOS runs its own event loop while the user drags a window edge, so
	// WaitEvent doesn't return; SDL delivers live-resize exposes to event
	// watchers on the main thread instead. The filter is created once:
	// purego never frees callbacks.
	watch := sdl.NewEventFilter(func(e *sdl.Event) bool {
		if e.Type == sdl.EVENT_WINDOW_EXPOSED && e.WindowEvent().Data1 == 1 {
			a.render()
		}
		return true
	})
	if err := sdl.AddEventWatch(watch); err != nil {
		return err
	}

	dirty := true
	for {
		animating := a.scene.animating()
		if dirty || animating {
			a.render()
			dirty = false
			animating = a.scene.animating()
		}
		var ev sdl.Event
		switch {
		case animating && a.stalled:
			// No swapchain texture (minimized or covered): don't spin.
			if !sdl.WaitEventTimeout(&ev, 50) {
				continue
			}
		case animating:
			if !sdl.PollEvent(&ev) {
				continue
			}
		default:
			if err := sdl.WaitEvent(&ev); err != nil {
				return fmt.Errorf("wait event: %w", err)
			}
		}
		for {
			switch ev.Type {
			case sdl.EVENT_QUIT, sdl.EVENT_WINDOW_CLOSE_REQUESTED:
				return quit()
			case sdl.EVENT_SYSTEM_THEME_CHANGED:
				a.pal = theme.For(a.dark())
				dirty = true
			case sdl.EVENT_USER:
				platform.WakeTaken()
				a.scene.woke()
				dirty = true
			case sdl.EVENT_WINDOW_DISPLAY_SCALE_CHANGED:
				// Glyphs are keyed by pixel size; drop the old scale's so
				// the atlas refills at the new one.
				if sc := a.window.Scale(); sc != a.scale {
					a.glyphs.Reset()
					a.scale = sc
				}
				dirty = true
			case sdl.EVENT_WINDOW_SHOWN, sdl.EVENT_WINDOW_RESTORED, sdl.EVENT_WINDOW_EXPOSED,
				sdl.EVENT_WINDOW_PIXEL_SIZE_CHANGED, sdl.EVENT_WINDOW_DISPLAY_CHANGED:
				dirty = true
			default:
				if a.scene.event(&ev, a.pal) {
					dirty = true
				}
			}
			if !sdl.PollEvent(&ev) {
				break
			}
		}
	}
}

// render draws and presents one frame.
func (a *winApp) render() {
	win := a.window
	cmd, err := a.device.AcquireCommandBuffer()
	if err != nil {
		slog.Error("acquire command buffer", "err", err)
		return
	}
	sc, err := cmd.WaitAndAcquireGPUSwapchainTexture(win.Handle())
	if err != nil || sc == nil || sc.Texture == nil {
		// Minimized or occluded: nothing to draw into.
		cmd.Submit()
		a.stalled = true
		return
	}
	a.stalled = false
	pw, ph := int(sc.Width), int(sc.Height)
	scale := win.Scale()
	w, h := float32(pw)/scale, float32(ph)/scale
	a.list.Reset()
	a.scene.draw(&a.list, w, h, a.pal, time.Now())
	a.batch.Reset(pw, ph)
	a.batch.AddList(&a.list, scale)
	if err := a.renderer.Prepare(cmd, &a.batch); err != nil {
		// Nothing was uploaded (dirty regions stay dirty for the next
		// frame); present a cleared frame rather than draw stale data.
		slog.Error("prepare", "err", err)
		a.batch.Reset(pw, ph)
	}
	if err := a.renderer.Draw(cmd, sc.Texture, win.SwapchainFormat(), pw, ph, a.pal.Background, &a.batch); err != nil {
		slog.Error("draw", "err", err)
	}
	if err := cmd.Submit(); err != nil {
		slog.Error("submit", "err", err)
	}
	a.frames.Add(1)
}

// flagSet reports whether the flag name was given.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
