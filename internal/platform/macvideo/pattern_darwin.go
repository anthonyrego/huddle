package macvideo

import (
	"fmt"
	"time"

	"github.com/anthonyrego/huddle/internal/video"
)

// PatternConfig sets up the test pattern as a source: what agents and
// bots send in place of a camera or a screen.
type PatternConfig struct {
	W, H, FPS int
	// FlashEvery lights the flash patch on every FlashEvery-th frame (from
	// frame FlashEvery on; 0 never), for lip-sync checks.
	FlashEvery int
	// Epoch is when frame 0 is due (zero: now). Frame n is due at Epoch
	// plus n frame intervals, and carries that time.
	Epoch time.Time
	// OnFrame, when set, is told of each frame before it goes on.
	OnFrame func(n uint32, at time.Time, flash bool)
}

// Flash reports whether frame n of the pattern flashes.
func (c PatternConfig) Flash(n uint32) bool {
	return c.FlashEvery > 0 && n > 0 && n%uint32(c.FlashEvery) == 0
}

// Pattern is the test pattern running as a source.
type Pattern struct {
	stop chan struct{}
	done chan struct{}
}

// StartPattern draws the pattern into buffers of its own at the frame
// rate, on a goroutine of its own, and hands each to fn.
func StartPattern(cfg PatternConfig, fn FrameFunc) (*Pattern, error) {
	if cfg.W <= 0 || cfg.H <= 0 || cfg.W%2 != 0 || cfg.H%2 != 0 || cfg.FPS <= 0 {
		return nil, fmt.Errorf("pattern: %dx%d at %d fps", cfg.W, cfg.H, cfg.FPS)
	}
	pool, err := NewPool(cfg.W, cfg.H)
	if err != nil {
		return nil, err
	}
	if cfg.Epoch.IsZero() {
		cfg.Epoch = time.Now()
	}
	p := &Pattern{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer pool.Close()
		interval := time.Second / time.Duration(cfg.FPS)
		t := time.NewTimer(0)
		defer t.Stop()
		for n := uint32(0); ; n++ {
			due := cfg.Epoch.Add(time.Duration(n) * interval)
			if late := time.Since(due); late > interval {
				n += uint32(late / interval) // fell behind: skip, as a camera would
				due = cfg.Epoch.Add(time.Duration(n) * interval)
			}
			t.Reset(time.Until(due))
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
			b, err := pool.Get()
			if err != nil {
				continue
			}
			flash := cfg.Flash(n)
			b.Edit(func(f video.Frame) { video.Draw(&f, n, flash) })
			if cfg.OnFrame != nil {
				cfg.OnFrame(n, due, flash)
			}
			fn(b, due)
		}
	}()
	return p, nil
}

// Stop stops the pattern and waits for its last frame.
func (p *Pattern) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}
