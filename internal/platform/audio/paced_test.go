package audio

import (
	"testing"
	"time"
)

func TestPacedSpreadsChunks(t *testing.T) {
	p := newPaced(48000, time.Second)
	t0 := time.Unix(100, 0)
	chunk := make([]float32, 4800) // 100 ms
	for i := range chunk {
		chunk[i] = float32(i)
	}
	p.push(chunk, t0)
	frame := make([]float32, 480)
	for j := range 10 {
		at := t0.Add(time.Duration(j) * 10 * time.Millisecond)
		if ok, wait := p.take(frame, at.Add(-time.Millisecond)); ok || wait != time.Millisecond {
			t.Fatalf("frame %d 1 ms early: ok %v, wait %v", j, ok, wait)
		}
		if ok, _ := p.take(frame, at); !ok || frame[0] != float32(j*480) {
			t.Fatalf("frame %d on time: ok %v, first sample %v", j, ok, frame[0])
		}
	}
	if ok, wait := p.take(frame, t0.Add(time.Second)); ok || wait != 0 {
		t.Fatalf("after the chunk: ok %v, wait %v; want to wait for a push", ok, wait)
	}
}

func TestPacedCatchesUp(t *testing.T) {
	p := newPaced(48000, time.Second)
	t0 := time.Unix(100, 0)
	p.push(make([]float32, 4800), t0)
	p.push(make([]float32, 4800), t0.Add(100*time.Millisecond))
	frame := make([]float32, 480)
	// A reader 150 ms late takes what's due at once, then paces again.
	now := t0.Add(150 * time.Millisecond)
	n := 0
	for {
		ok, _ := p.take(frame, now)
		if !ok {
			break
		}
		n++
	}
	if n != 16 {
		t.Fatalf("took %d frames at 150 ms, want 16 (all of the first chunk and 60 ms of the second)", n)
	}
}

func TestPacedDropsOldest(t *testing.T) {
	p := newPaced(48000, 200*time.Millisecond)
	t0 := time.Unix(100, 0)
	for i := range 5 {
		c := make([]float32, 4800)
		c[0] = float32(i + 1)
		p.push(c, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	if h := p.held(); h != 9600 {
		t.Fatalf("held %d, want 9600 (200 ms)", h)
	}
	frame := make([]float32, 480)
	if ok, _ := p.take(frame, t0.Add(time.Second)); !ok || frame[0] != 4 {
		t.Fatalf("oldest kept starts %v, want chunk 4", frame[0])
	}
}
