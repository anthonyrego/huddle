package room

import (
	"context"
	"net"
	"testing"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

// TestDialer: a join with no media named, or LiveKit, goes through lksdk;
// builtin through sfuclient; anything else is refused before anything is
// dialled.
func TestDialer(t *testing.T) {
	for _, kind := range []string{"", wire.MediaLiveKit, wire.MediaBuiltin} {
		if d, err := dialer(kind); d == nil || err != nil {
			t.Fatalf("media %q: %v", kind, err)
		}
	}
	if _, err := dialer("sip"); err == nil {
		t.Fatal("an unknown media server was taken")
	}
}

// A builtin media server that can't be reached reads as LiveKit's does in
// the app: "join the voice room: could not … : dial tcp …".
func TestBuiltinUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = (&Connector{}).Connect(context.Background(), rtc.Join{Kind: wire.MediaBuiltin, URL: "ws://" + addr + "/rtc", Token: "t"}, rtc.Events{})
	want := "join the voice room: could not reach the media server: dial tcp " + addr + ": connect: connection refused"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

func TestLoopbackURL(t *testing.T) {
	for u, want := range map[string]bool{
		"ws://127.0.0.1:8092/rtc":    true,
		"ws://localhost:8092/rtc":    true,
		"ws://[::1]:8092/rtc":        true,
		"ws://10.10.70.162:8092/rtc": false,
		"wss://huddle.example/rtc":   false,
		"::bad":                      false,
	} {
		if got := loopbackURL(u); got != want {
			t.Errorf("loopbackURL(%q) = %v", u, got)
		}
	}
}
