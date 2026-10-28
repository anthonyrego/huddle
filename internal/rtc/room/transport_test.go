package room

import (
	"context"
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

// TestDialer: a join with no media named, or LiveKit, goes through lksdk;
// builtin says it isn't here yet, before anything is dialled.
func TestDialer(t *testing.T) {
	for _, kind := range []string{"", wire.MediaLiveKit} {
		if d, err := dialer(kind); d == nil || err != nil {
			t.Fatalf("media %q: %v", kind, err)
		}
	}
	if _, err := dialer("sip"); err == nil {
		t.Fatal("an unknown media server was taken")
	}
	_, err := (&Connector{}).Connect(context.Background(), rtc.Join{Kind: wire.MediaBuiltin, URL: "ws://127.0.0.1:1/rtc"}, rtc.Events{})
	if err == nil || !strings.Contains(err.Error(), "builtin media isn't in this build yet") {
		t.Fatalf("builtin: %v", err)
	}
}
