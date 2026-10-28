// Package platform owns process-level SDL setup. Only the app and the
// offscreen renderer import it; the pure core never does.
//
// Copied from an earlier project of the author's.
package platform

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/Zyko0/go-sdl3/bin/binsdl"
	"github.com/Zyko0/go-sdl3/sdl"
)

var loadOnce sync.Once

// LoadSDL loads the SDL3 shared library: the app bundle's own copy
// (Contents/Frameworks, signed with the app, which the hardened runtime
// requires), else a system install, else the copy embedded in the binary
// (dev runs outside a bundle). Safe to call repeatedly.
func LoadSDL() {
	loadOnce.Do(func() {
		if p := bundledSDL(); p != "" && sdl.LoadLibrary(p) == nil {
			return
		}
		if err := sdl.LoadLibrary(sdl.Path()); err != nil {
			binsdl.Load()
		}
	})
}

// bundledSDL is the SDL library in the app bundle around this executable,
// or "" when there is none.
func bundledSDL() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "..", "Frameworks", sdl.Path())
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Mode picks how SDL presents the process to the OS.
type Mode int

const (
	// Foreground is the app: a normal windowed process.
	Foreground Mode = iota
	// Background is for offscreen rendering from the CLI: no
	// Dock icon and no focus change on macOS. Metal still needs the video
	// subsystem, so SDL_INIT_VIDEO is used either way.
	Background
)

// foregroundHints configure the app's window.
var foregroundHints = [][2]string{
	// The window takes focus when it appears: huddle is opened on purpose.
	{sdl.HINT_WINDOW_ACTIVATE_WHEN_SHOWN, "1"},
	// Trackpad momentum arrives as wheel events, so a flick keeps
	// scrolling like every other Mac app.
	{sdl.HINT_MAC_SCROLL_MOMENTUM, "1"},
	// A click on the window while the terminal has focus acts on what's
	// under it (a tab, a divider) instead of only activating the window.
	{sdl.HINT_MOUSE_FOCUS_CLICKTHROUGH, "1"},
}

// Init loads SDL and initializes the video subsystem. Must run on the main
// thread (go-sdl3 locks it in its init).
//
// SDL's own SIGINT/SIGTERM handlers are disabled: they only set a flag and
// never wake a blocked WaitEvent. Callers that run an event loop use
// QuitOnSignal instead.
func Init(mode Mode) error {
	LoadSDL()
	if err := sdl.SetHint(sdl.HINT_NO_SIGNAL_HANDLERS, "1"); err != nil {
		return fmt.Errorf("set signal hint: %w", err)
	}
	if mode == Background {
		if err := sdl.SetHint(sdl.HINT_MAC_BACKGROUND_APP, "1"); err != nil {
			return fmt.Errorf("set background hint: %w", err)
		}
	} else {
		for _, h := range foregroundHints {
			if err := sdl.SetHint(h[0], h[1]); err != nil {
				return fmt.Errorf("set hint %s: %w", h[0], err)
			}
		}
	}
	if err := sdl.Init(sdl.INIT_VIDEO); err != nil {
		return fmt.Errorf("SDL_Init: %w", err)
	}
	return nil
}

// InitAudio loads SDL and initializes only the audio subsystem, for the
// voice CLI that opens no window. SDL's signal handlers are disabled as in
// Init.
func InitAudio() error {
	LoadSDL()
	if err := sdl.SetHint(sdl.HINT_NO_SIGNAL_HANDLERS, "1"); err != nil {
		return fmt.Errorf("set signal hint: %w", err)
	}
	if err := sdl.InitSubSystem(sdl.INIT_AUDIO); err != nil {
		return fmt.Errorf("SDL_InitSubSystem(audio): %w", err)
	}
	return nil
}

// Quit shuts SDL down.
func Quit() { sdl.Quit() }

// QuitOnSignal turns SIGINT and SIGTERM into an SDL quit event, which wakes a
// blocked WaitEvent from this goroutine. PushEvent is safe from any thread.
func QuitOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range ch {
			sdl.PushEvent(&sdl.Event{Type: sdl.EVENT_QUIT})
		}
	}()
}

// wakePending merges wakes: while one EVENT_USER is queued and not yet taken
// by the loop, further Wake calls push nothing. Watchers publish a snapshot
// and then wake, so one pending event covers any number of publishes.
var wakePending atomic.Bool

// pushWake queues the EVENT_USER. A variable so tests can run without SDL.
var pushWake = func() error { return sdl.PushEvent(&sdl.Event{Type: sdl.EVENT_USER}) }

// Wake wakes the main loop from any goroutine. Wakes are merged through an
// atomic flag, so a watcher that publishes many snapshots between frames
// queues one event, not one per publish.
func Wake() error {
	if !wakePending.CompareAndSwap(false, true) {
		return nil
	}
	if err := pushWake(); err != nil {
		wakePending.Store(false)
		return err
	}
	return nil
}

// WakeTaken tells Wake that the loop has taken the pending EVENT_USER. Call
// it before reading the state the wake announced: a publish that lands after
// this call queues a fresh event, so it is never lost.
func WakeTaken() { wakePending.Store(false) }
