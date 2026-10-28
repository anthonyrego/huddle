package macvideo

import (
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// TestBitrateSwings: a layer whose bitrate rises (a probe), is squeezed
// (congestion) and comes back in steps (as the room gives it back) keeps
// its frame rate. VideoToolbox's low-latency rate control doesn't: it
// drops to 17–20 fps at 255 kbps and stays there.
func TestBitrateSwings(t *testing.T) {
	s, err := NewSender(SenderConfig{Layers: []rtc.Layer{{W: 640, H: 360, Bitrate: 450_000}}, FPS: 30})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	perSec := map[int]int{}
	start := time.Now()
	if err := s.Start(func(_ int, _ []byte, pts time.Duration) {
		mu.Lock()
		perSec[int(pts/time.Second)]++
		mu.Unlock()
	}); err != nil {
		if st, ok := err.(*OSStatusError); ok && st.Status == couldNotFindEncoder {
			t.Skip("no H.264 encoder here")
		}
		t.Fatal(err)
	}
	pool, err := NewPool(1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	const secs = 8
	for i := range 30 * secs {
		<-tick.C
		switch {
		case i >= 30 && i < 90 && i%15 == 0:
			s.SetBitrates([]int{450_000 + (i-15)*4000}) // up to ~750 kbps
		case i == 90:
			s.SetBitrates([]int{255_000})
		case i >= 120 && i <= 180 && i%15 == 0:
			s.SetBitrates([]int{255_000 + (i-120)*3250}) // back up in steps
		}
		b, err := pool.Get()
		if err != nil {
			t.Fatal(err)
		}
		b.Edit(func(f video.Frame) { video.Draw(&f, uint32(i), false) })
		s.Feed(b, start.Add(time.Duration(i)*time.Second/30))
		b.Release()
	}
	s.Stop()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("frames out per second %v", perSec)
	for sec := range secs {
		if perSec[sec] < 27 {
			t.Errorf("second %d: %d frames", sec, perSec[sec])
		}
	}
}
