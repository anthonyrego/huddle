package room

import (
	"context"
	"net"
	"testing"

	"github.com/anthonyrego/huddle/internal/rtc"
)

// A media server that can't be reached reads in the app as "join the voice
// room: could not reach the media server: dial tcp …".
func TestBuiltinUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = (&Connector{}).Connect(context.Background(), rtc.Join{URL: "ws://" + addr + "/rtc", Token: "t"}, rtc.Events{})
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
		"ws://192.168.1.20:8092/rtc": false,
		"wss://huddle.example/rtc":   false,
		"::bad":                      false,
	} {
		if got := loopbackURL(u); got != want {
			t.Errorf("loopbackURL(%q) = %v", u, got)
		}
	}
}
