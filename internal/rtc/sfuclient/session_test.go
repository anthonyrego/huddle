package sfuclient

// The session against a real SFU is tested in internal/server/sfu (the
// server and the client are built together, end to end on loopback).

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// Events come in the order pushed, one at a time; finish runs what's
// queued, then its last event, then nothing; stop drops what's queued.
func TestQueue(t *testing.T) {
	q := newQueue()
	var mu sync.Mutex
	var got []int
	add := func(n int) func() {
		return func() {
			mu.Lock()
			got = append(got, n)
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}
	done := make(chan struct{})
	go func() {
		q.run()
		close(done)
	}()
	for i := range 20 {
		q.push(add(i))
	}
	q.finish(add(100))
	q.push(add(200))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue didn't finish")
	}
	want := []int{}
	for i := range 20 {
		want = append(want, i)
	}
	want = append(want, 100)
	if !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}

	q = newQueue()
	got = nil
	block := make(chan struct{})
	q.push(func() { <-block })
	q.push(add(1))
	done = make(chan struct{})
	go func() {
		q.run()
		close(done)
	}()
	q.stop()
	close(block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue didn't stop")
	}
	if len(got) != 0 {
		t.Errorf("ran %v after stop", got)
	}
}
