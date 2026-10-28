package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/wire"
)

// waitFor polls cond until it holds or the time runs out.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// closedAddr is a local address nothing listens on.
func closedAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func (mt *meeting) peek() meetingState {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.m
}

func TestConnSaysWhyItIsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Nothing listening: the server can't be reached, and the bar still
	// names the room and the server.
	addr := closedAddr(t)
	mt := &meeting{}
	go newConn("http://"+addr, "design-review", "Ann", mt).run(ctx)
	waitFor(t, "unreachable", 3*time.Second, func() bool { return mt.peek().conn == connUnreachable })
	s := mt.peek()
	if s.retryIn < 1 || s.server != addr || s.room != "design-review" {
		t.Fatalf("unreachable: retryIn %d, server %q, room %q", s.retryIn, s.server, s.room)
	}

	// An answer that's an error: the server didn't let us in.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	mt2 := &meeting{}
	go newConn(srv.URL, "design-review", "Ann", mt2).run(ctx)
	waitFor(t, "refused", 3*time.Second, func() bool { return mt2.peek().conn == connRefused })
}

func TestCountdown(t *testing.T) {
	var ticks []int
	if !countdown(context.Background(), 2500*time.Millisecond, func(n int) { ticks = append(ticks, n) }) {
		t.Fatal("countdown ended early")
	}
	if len(ticks) != 3 || ticks[0] != 3 || ticks[2] != 1 {
		t.Fatalf("ticks %v, want 3 2 1", ticks)
	}
}

// Voice retries on its own when LiveKit can't be reached, takes a new join
// at once, and asks for one when its token is too old to join with.
func TestVoiceRetries(t *testing.T) {
	old := tokenFresh
	tokenFresh = 0
	defer func() { tokenFresh = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newCall(true, "", nil)
	rejoins := make(chan struct{}, 4)
	c.rejoin = func() { rejoins <- struct{}{} }
	go c.run(ctx)
	if st := c.snapshot(); st.status != callWaiting {
		t.Fatalf("before a join: %q", st.status)
	}
	c.onJoin(wire.Joined{Room: "r", Identity: "ann-1", LiveKitURL: "ws://" + closedAddr(t), Token: "x"})
	waitFor(t, "a retry", 10*time.Second, func() bool { return c.snapshot().status == callRetry })
	st := c.snapshot()
	if st.retryIn < 1 || st.canMute || !strings.Contains(st.lostErr, "join the voice room") {
		t.Fatalf("retrying: in %d, canMute %v, err %q", st.retryIn, st.canMute, st.lostErr)
	}
	// The token counts as old at once here: after the wait voice asks the
	// connection to join again, and waits for it.
	select {
	case <-rejoins:
	case <-time.After(5 * time.Second):
		t.Fatal("no rejoin asked for")
	}
	waitFor(t, "waiting for the server", 2*time.Second, func() bool { return c.snapshot().status == callWaiting })
	c.onJoin(wire.Joined{Room: "r", Identity: "ann-2", LiveKitURL: "ws://" + closedAddr(t), Token: "y"})
	waitFor(t, "the new join tried", 10*time.Second, func() bool {
		s := c.snapshot()
		return s.status == callRetry || s.status == callJoining
	})
}

// Voice retries the same way when huddled's own media server can't be
// reached, and says so in the same words.
func TestVoiceRetriesBuiltin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newCall(true, "", nil)
	go c.run(ctx)
	addr := closedAddr(t)
	c.onJoin(wire.Joined{Room: "r", Identity: "ann-1", Media: wire.MediaBuiltin, MediaURL: "ws://" + addr + "/rtc", Token: "x"})
	waitFor(t, "a retry", 10*time.Second, func() bool { return c.snapshot().status == callRetry })
	st := c.snapshot()
	want := "join the voice room: could not reach the media server: dial tcp " + addr + ": connect: connection refused"
	if st.retryIn < 1 || st.canMute || st.lostErr != want {
		t.Fatalf("retrying: in %d, canMute %v, err %q", st.retryIn, st.canMute, st.lostErr)
	}
}
