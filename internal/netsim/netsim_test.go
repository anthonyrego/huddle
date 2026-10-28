package netsim

import (
	"sync"
	"testing"
	"time"
)

func TestFate(t *testing.T) {
	s := New(0.10, 40*time.Millisecond, 0, 1)
	t0 := time.Unix(0, 0)
	dropped, last := 0, time.Time{}
	var maxDelay time.Duration
	for i := range 10000 {
		at := t0.Add(time.Duration(i) * 20 * time.Millisecond)
		d, drop := s.Fate(at)
		if drop {
			dropped++
			continue
		}
		if d.Before(last) {
			t.Fatalf("packet %d delivered before the one ahead of it", i)
		}
		last = d
		maxDelay = max(maxDelay, d.Sub(at))
	}
	if dropped < 900 || dropped > 1100 {
		t.Fatalf("dropped %d of 10000 at 10%%", dropped)
	}
	if maxDelay > 100*time.Millisecond || maxDelay < 70*time.Millisecond {
		t.Fatalf("most delay %v with 40 ms jitter, want under 2J plus a frame", maxDelay)
	}
	r := New(0, 0, 0.5, 2)
	reordered, prev := 0, time.Time{}
	for i := range 1000 {
		d, _ := r.Fate(t0.Add(time.Duration(i) * 20 * time.Millisecond))
		if d.Before(prev) {
			reordered++
		}
		prev = d
	}
	if reordered < 100 {
		t.Fatalf("%d of 1000 overtaken at 50%% reorder", reordered)
	}
}

func TestPipe(t *testing.T) {
	var mu sync.Mutex
	var got []int
	p := NewPipe(New(0, 5*time.Millisecond, 0, 3), func(v int) { mu.Lock(); got = append(got, v); mu.Unlock() })
	defer p.Close()
	for i := range 20 {
		p.Send(i)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 20 {
		t.Fatalf("delivered %d of 20", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("delivered out of order: %v", got)
		}
	}
}
