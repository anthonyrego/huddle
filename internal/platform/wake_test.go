package platform

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWakeMergesUntilTaken(t *testing.T) {
	var pushed atomic.Int32
	defer func(old func() error) { pushWake = old; wakePending.Store(false) }(pushWake)
	pushWake = func() error { pushed.Add(1); return nil }

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() { defer wg.Done(); Wake() }()
	}
	wg.Wait()
	if n := pushed.Load(); n != 1 {
		t.Fatalf("64 concurrent wakes pushed %d events, want 1", n)
	}

	WakeTaken()
	Wake()
	Wake()
	if n := pushed.Load(); n != 2 {
		t.Fatalf("after WakeTaken, pushed %d events in total, want 2", n)
	}
}

func TestWakeRetriesAfterPushFailure(t *testing.T) {
	defer func(old func() error) { pushWake = old; wakePending.Store(false) }(pushWake)
	fail := true
	var pushed int
	pushWake = func() error {
		pushed++
		if fail {
			return errors.New("queue full")
		}
		return nil
	}
	if err := Wake(); err == nil {
		t.Fatal("want the push error")
	}
	fail = false
	if err := Wake(); err != nil || pushed != 2 {
		t.Fatalf("second wake: err=%v pushed=%d, want a retry", err, pushed)
	}
}
