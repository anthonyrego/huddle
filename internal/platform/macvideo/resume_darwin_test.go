package macvideo

import (
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// TestResumeRate: a layer skipped for 6 s (bitrate 0, as Dynacast pauses
// one nobody is sent) and given 680 kbps again starts with a keyframe and
// sends about what a layer never skipped sends at that rate (VideoToolbox
// keeps a second within half again of it: ~1 Mbps on the pattern), from
// its first second, its access units keeping the times they were fed at.
// Without Encoder.Skipped it sent 1.7–1.9 Mbps for 4 s.
func TestResumeRate(t *testing.T) {
	s, err := NewSender(SenderConfig{Layers: []rtc.Layer{{W: 1280, H: 720, Bitrate: 1_700_000}}, FPS: 30})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	bytes := map[int]int{}
	var keys []time.Duration
	last, backwards := time.Duration(-1), 0
	if err := s.Start(func(_ int, au []byte, pts time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		bytes[int((pts+time.Millisecond)/time.Second)] += len(au)
		if video.IsKeyframe(au) {
			keys = append(keys, pts)
		}
		if pts <= last {
			backwards++
		}
		last = pts
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
	const (
		pauseAt  = 3
		resumeAt = 9
		secs     = 12
	)
	start := s.start // pts 0
	for i := range 30 * secs {
		<-tick.C
		switch i {
		case 30 * pauseAt:
			s.SetBitrates([]int{0})
		case 30 * resumeAt:
			s.SetBitrates([]int{680_000})
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
	for sec := range secs {
		t.Logf("second %2d: %4d kbit", sec, bytes[sec]*8/1000)
	}
	t.Logf("keyframes at %v", keys)
	for sec := resumeAt; sec < secs; sec++ {
		if kbps := bytes[sec] * 8 / 1000; kbps > 1200 {
			t.Errorf("second %d after starting again at 680 kbps: %d kbit", sec-resumeAt, kbps)
		}
	}
	resumed := false
	for _, k := range keys {
		resumed = resumed || k.Round(time.Millisecond) == resumeAt*time.Second
	}
	if !resumed || backwards > 0 {
		t.Errorf("keyframes at %v (want one at %v); %d timestamps going back", keys, resumeAt*time.Second, backwards)
	}
}
