package cue

import (
	"testing"
	"time"
)

func TestQueueWaitsForPlaybackAndSpacesReleases(t *testing.T) {
	t0 := time.Unix(0, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	q := Queue[string]{Lead: 100, Gap: 180 * time.Millisecond}
	q.Push(1000, "a")
	q.Push(1000, "b")
	q.Push(5000, "c")

	if _, ok := q.Next(at(0), 800, false); ok {
		t.Fatal("released before playback came within the lead")
	}
	if v, ok := q.Next(at(10), 900, false); !ok || v != "a" {
		t.Fatalf("got %q %v, want a (900+100 reaches 1000)", v, ok)
	}
	if _, ok := q.Next(at(100), 2000, false); ok {
		t.Fatal("released b inside the gap")
	}
	if v, ok := q.Next(at(190), 2000, false); !ok || v != "b" {
		t.Fatalf("got %q %v, want b after the gap", v, ok)
	}
	if _, ok := q.Next(at(400), 2000, false); ok {
		t.Fatal("released c before its audio")
	}
	// Playback ran dry: what's left was heard (or never audio).
	if v, ok := q.Next(at(410), 2000, true); !ok || v != "c" {
		t.Fatalf("got %q %v, want c once drained", v, ok)
	}
	if q.Len() != 0 {
		t.Fatalf("Len = %d", q.Len())
	}
}

func TestHoldAndDrop(t *testing.T) {
	t0 := time.Unix(0, 0)
	q := Queue[int]{}
	for i := range 4 {
		q.Push(0, i)
	}
	q.HoldUntil(t0.Add(300 * time.Millisecond))
	if _, ok := q.Next(t0.Add(100*time.Millisecond), 0, true); ok {
		t.Fatal("released during the hold")
	}
	dropped := q.Drop(func(v int) bool { return v%2 == 1 })
	if len(dropped) != 2 || dropped[0] != 1 || dropped[1] != 3 {
		t.Fatalf("dropped %v", dropped)
	}
	if v, ok := q.Next(t0.Add(300*time.Millisecond), 0, true); !ok || v != 0 {
		t.Fatalf("got %d %v after the hold", v, ok)
	}
	if v, _ := q.Next(t0.Add(301*time.Millisecond), 0, true); v != 2 {
		t.Fatalf("got %d, want 2 (Gap 0)", v)
	}
}
