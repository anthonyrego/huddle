package fixturemeet

import (
	"context"
	"sync"
	"testing"
	"time"
)

// frames records what a Track plays.
type frames struct {
	mu     sync.Mutex
	pkts   [][]byte
	tss    []uint32
	levels []uint8
	at     []time.Time
}

func (f *frames) WriteFrame(frame []byte, ts uint32, level *uint8) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pkts = append(f.pkts, frame)
	f.tss = append(f.tss, ts)
	f.levels = append(f.levels, *level)
	f.at = append(f.at, time.Now())
	return nil
}

// Play writes each packet with its level, at RTP timestamps that move on by
// each packet's length, in real time from t0, and loops when asked.
func TestPlay(t *testing.T) {
	tr := &Track{
		// Opus TOC bytes: 20 ms of CELT, then 10 ms.
		pkts:   [][]byte{{0xf8, 1}, {0xf0, 2}, {0xf8, 3}},
		levels: []uint8{30, 40, 127},
	}
	var f frames
	t0 := time.Now().Add(20 * time.Millisecond)
	if err := tr.Play(context.Background(), &f, t0, false); err != nil {
		t.Fatal(err)
	}
	if len(f.pkts) != 3 || f.pkts[1][1] != 2 || f.levels[0] != 30 || f.levels[2] != 127 {
		t.Fatalf("played %v with levels %v", f.pkts, f.levels)
	}
	if d1, d2 := f.tss[1]-f.tss[0], f.tss[2]-f.tss[1]; d1 != 960 || d2 != 480 {
		t.Errorf("timestamps moved on by %d and %d, want 960 and 480", d1, d2)
	}
	if f.at[0].Before(t0) || f.at[2].Sub(f.at[0]) < 29*time.Millisecond {
		t.Errorf("not in real time: %v after t0, the last %v after the first", f.at[0].Sub(t0), f.at[2].Sub(f.at[0]))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var g frames
	if err := tr.Play(ctx, &g, time.Now(), true); err != context.DeadlineExceeded {
		t.Fatalf("a loop ended with %v", err)
	}
	if len(g.pkts) < 6 {
		t.Errorf("looped %d packets in 150 ms", len(g.pkts))
	}
}
