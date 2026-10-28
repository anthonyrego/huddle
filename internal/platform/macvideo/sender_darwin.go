package macvideo

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc"
)

// SenderConfig sets up a Sender.
type SenderConfig struct {
	Layers []rtc.Layer // largest first
	FPS    int         // the frame rate the encoders expect
	// KeyEvery is the longest between keyframes (default 5 s); receivers
	// ask for one whenever they need it, so this is a safety net.
	KeyEvery time.Duration
	// Scale makes each smaller layer's picture with a Scaler (one
	// VTPixelTransferSession per layer) instead of handing every encoder
	// the full-size picture to scale itself.
	Scale bool
	// Hold keeps the newest picture, to encode again when a keyframe is
	// asked for while nothing new comes (a still screen sends nothing).
	Hold bool
}

// Sender is a video source for the room (rtc.VideoSource): pictures fed
// to it (from the camera, a share, the pattern) are encoded at every
// layer congestion control has left running, each by its own encoder, and
// written out in order from a goroutine of its own (never VideoToolbox's
// thread). It can be started and stopped again, as the call reconnects.
type Sender struct {
	cfg SenderConfig

	mu      sync.RWMutex // held for reading while encoding, for writing to start and stop
	encs    []*Encoder
	scalers []*Scaler
	write   func(layer int, au []byte, pts time.Duration)
	out     chan layerAU
	wg      sync.WaitGroup
	start   time.Time
	lastPTS time.Duration
	running bool
	quit    chan struct{}

	bps    []atomic.Int64
	force  []atomic.Bool
	fed    atomic.Int64 // pictures taken
	lost   atomic.Int64 // access units the writer couldn't take
	held   *Buffer
	heldAt time.Time
	holdMu sync.Mutex
}

type layerAU struct {
	layer int
	e     Encoded
}

// NewSender makes a sender; it encodes nothing until started.
func NewSender(cfg SenderConfig) (*Sender, error) {
	if len(cfg.Layers) == 0 {
		return nil, errors.New("sender: no layers")
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	if cfg.KeyEvery == 0 {
		cfg.KeyEvery = 5 * time.Second
	}
	s := &Sender{cfg: cfg, bps: make([]atomic.Int64, len(cfg.Layers)), force: make([]atomic.Bool, len(cfg.Layers))}
	for i, l := range cfg.Layers {
		s.bps[i].Store(int64(l.Bitrate))
	}
	return s, nil
}

// Layers are the layers it sends, largest first.
func (s *Sender) Layers() []rtc.Layer { return s.cfg.Layers }

// Start makes the encoders and starts writing.
func (s *Sender) Start(write func(layer int, au []byte, pts time.Duration)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("sender: already started")
	}
	s.out = make(chan layerAU, 32)
	s.write, s.start, s.lastPTS = write, time.Now(), 0
	out := s.out
	for i, l := range s.cfg.Layers {
		i := i
		bps := int(s.bps[i].Load())
		if bps == 0 {
			bps = l.Bitrate
		}
		// Not the low-latency rate control: squeezed (a 360p layer at 255
		// kbps) it drops frames, and given its bitrate back in steps it
		// stays at 17 fps; the ordinary real-time one holds 30 fps through
		// the same swings (TestBitrateSwings) for 0.7 ms more per frame.
		e, err := NewEncoder(EncoderConfig{W: l.W, H: l.H, FPS: s.cfg.FPS, Bitrate: bps, KeyEvery: s.cfg.KeyEvery, NoLowLatency: true}, func(e Encoded) {
			select {
			case out <- layerAU{i, e}:
			default: // the writer is far behind: this layer starts again at a keyframe
				s.lost.Add(1)
				s.force[i].Store(true)
			}
		})
		if err != nil {
			s.closeEncoders()
			return fmt.Errorf("sender: layer %dx%d: %w", l.W, l.H, err)
		}
		s.encs = append(s.encs, e)
		if s.cfg.Scale && i > 0 {
			sc, err := NewScaler()
			if err != nil {
				s.closeEncoders()
				return err
			}
			s.scalers = append(s.scalers, sc)
		} else {
			s.scalers = append(s.scalers, nil)
		}
		s.force[i].Store(true)
	}
	s.running = true
	s.quit = make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for a := range out {
			write(a.layer, a.e.Data, a.e.PTS)
		}
	}()
	if s.cfg.Hold {
		s.wg.Add(1)
		go s.repeat(s.quit)
	}
	return nil
}

// Feed encodes one picture at every running layer. b stays the caller's
// (the encoders retain what they keep).
func (s *Sender) Feed(b *Buffer, at time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.running {
		return
	}
	s.fed.Add(1)
	pts := at.Sub(s.start)
	s.holdMu.Lock()
	if pts <= s.lastPTS {
		pts = s.lastPTS + time.Millisecond
	}
	s.lastPTS = pts
	if s.cfg.Hold && b != s.held {
		if s.held != nil {
			s.held.Release()
		}
		s.held, s.heldAt = b.Retain(), time.Now()
	}
	s.holdMu.Unlock()
	for i, e := range s.encs {
		if s.bps[i].Load() == 0 {
			continue
		}
		src := b
		if sc := s.scalers[i]; sc != nil {
			dst, err := e.NewBuffer()
			if err != nil {
				continue
			}
			if err := sc.Scale(b, dst); err != nil {
				dst.Release()
				continue
			}
			src = dst
		}
		if err := e.Encode(src, pts, s.force[i].Swap(false)); err != nil {
			slog.Debug("sender: encode", "layer", i, "err", err)
		}
		if src != b {
			src.Release()
		}
	}
}

// repeat encodes the held picture again when a keyframe is asked for and
// nothing new has come for a while.
func (s *Sender) repeat(quit chan struct{}) {
	defer s.wg.Done()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-quit:
			return
		case <-t.C:
		}
		asked := false
		for i := range s.force {
			asked = asked || (s.force[i].Load() && s.bps[i].Load() > 0)
		}
		s.holdMu.Lock()
		b := s.held
		stale := b != nil && time.Since(s.heldAt) > 300*time.Millisecond
		if stale && asked {
			b = b.Retain()
		}
		s.holdMu.Unlock()
		if stale && asked {
			s.Feed(b, time.Now())
			b.Release()
		}
	}
}

// KeyFrame makes the layer's next picture a keyframe.
func (s *Sender) KeyFrame(layer int) {
	if layer >= 0 && layer < len(s.force) {
		s.force[layer].Store(true)
	}
}

// SetBitrates gives each layer what congestion control allows; 0 skips
// the layer. A layer that comes back starts with a keyframe.
func (s *Sender) SetBitrates(bps []int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range min(len(bps), len(s.bps)) {
		was := int(s.bps[i].Load())
		if was == bps[i] {
			continue
		}
		s.bps[i].Store(int64(bps[i]))
		if bps[i] == 0 {
			continue
		}
		if was == 0 {
			s.force[i].Store(true)
		}
		if s.running && i < len(s.encs) && (was == 0 || math.Abs(float64(bps[i]-was)) > 0.05*float64(was)) {
			s.encs[i].SetBitrate(bps[i])
		}
	}
}

// Stop flushes and closes the encoders and waits for the writer.
func (s *Sender) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.quit)
	s.closeEncoders()
	close(s.out) // the encoders are closed: no more output
	s.mu.Unlock()
	s.wg.Wait()
	s.holdMu.Lock()
	if s.held != nil {
		s.held.Release()
		s.held = nil
	}
	s.holdMu.Unlock()
}

func (s *Sender) closeEncoders() {
	for _, e := range s.encs {
		e.Close()
	}
	for _, sc := range s.scalers {
		if sc != nil {
			sc.Close()
		}
	}
	s.encs, s.scalers = nil, nil
}

// SenderStats are a sender's numbers.
type SenderStats struct {
	Fed     int64 // pictures taken
	Lost    int64 // access units the writer fell too far behind for
	Dropped []int // frames each encoder dropped
}

// Stats are the sender's numbers so far.
func (s *Sender) Stats() SenderStats {
	st := SenderStats{Fed: s.fed.Load(), Lost: s.lost.Load()}
	s.mu.RLock()
	for _, e := range s.encs {
		st.Dropped = append(st.Dropped, e.Dropped())
	}
	s.mu.RUnlock()
	return st
}

// CameraLayers are a camera's layers for a picture of w×h: the picture
// (at most 1280 wide), then half and a quarter of it, with bitrates in
// proportion to 720p's 1.7 Mbps, 360p's 450 kbps and 180p's 160 kbps.
func CameraLayers(w, h int) []rtc.Layer {
	top := fit(w, h, 1280)
	return layers(top, []int{1_700_000, 450_000, 160_000}, []int{1280 * 720, 640 * 360, 320 * 180}, 3)
}

// ScreenLayers are a screen's layers for a picture of w×h: the picture
// (at most 1920 wide), at 2.5 Mbps for 1920×1080, and half of it.
func ScreenLayers(w, h int) []rtc.Layer {
	top := fit(w, h, ShareMaxWidth)
	return layers(top, []int{2_500_000, 800_000}, []int{1920 * 1080, 960 * 540}, 2)
}

// fit scales w×h down to at most maxW wide, even.
func fit(w, h, maxW int) [2]int {
	if w > maxW {
		h = h * maxW / w
		w = maxW
	}
	return [2]int{w &^ 1, h &^ 1}
}

// layers halves top n-1 times, giving each its bitrate scaled by area
// (no layer under 64 pixels a side).
func layers(top [2]int, rates, areas []int, n int) []rtc.Layer {
	var out []rtc.Layer
	for i := range n {
		w, h := (top[0]>>i)&^1, (top[1]>>i)&^1
		if w < 64 || h < 64 {
			break
		}
		bps := int(float64(rates[i]) * float64(w*h) / float64(areas[i]))
		out = append(out, rtc.Layer{W: w, H: h, Bitrate: max(bps, 50_000)})
	}
	return out
}
