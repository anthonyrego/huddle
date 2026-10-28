package main

import (
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/wire"
)

func TestMediaLiveKit(t *testing.T) {
	t.Setenv("HUDDLE_MEDIA", "")
	t.Setenv("HUDDLE_NODE_IP", "192.0.2.7")
	t.Setenv("LIVEKIT_URL", "ws://lk:7880")
	t.Setenv("LIVEKIT_API_KEY", "devkey")
	t.Setenv("LIVEKIT_API_SECRET", "secret")
	m, err := newMedia(":8080")
	if err != nil {
		t.Fatal(err)
	}
	if lk, ok := m.agent.(agent.LiveKit); !ok || lk.URL != "ws://lk:7880" || lk.Minter.Key != "devkey" {
		t.Fatalf("the whiteboard's media: %#v", m.agent)
	}
	var s httpapi.Server
	m.api(&s)
	if s.Media != wire.MediaLiveKit || s.LiveKitURL != "ws://lk:7880" || s.Minter.Secret != "secret" || s.RTC != nil {
		t.Fatalf("the API's media: %+v", s)
	}
	if got := m.attrs(); len(got) != 6 || got[3] != "192.0.2.7" {
		t.Fatalf("logged as %v", got)
	}

	t.Setenv("LIVEKIT_API_SECRET", "")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "LIVEKIT_API_SECRET") {
		t.Fatalf("no LiveKit secret: %v", err)
	}
}

func TestMediaBuiltin(t *testing.T) {
	// No LiveKit needed; until the SFU is in, it says so.
	t.Setenv("HUDDLE_MEDIA", "builtin")
	t.Setenv("LIVEKIT_URL", "")
	t.Setenv("HUDDLE_RTC_SECRET", "")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "needs the SFU") {
		t.Fatalf("builtin: %v", err)
	}
	t.Setenv("HUDDLE_RTC_SECRET", "short")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "HUDDLE_RTC_SECRET") {
		t.Fatalf("a short secret: %v", err)
	}
	t.Setenv("HUDDLE_NODE_IP", "not-an-ip")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "HUDDLE_NODE_IP") {
		t.Fatalf("a node IP that isn't one: %v", err)
	}
	t.Setenv("HUDDLE_MEDIA", "carrier-pigeon")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "livekit or builtin") {
		t.Fatalf("an unknown media server: %v", err)
	}
}

func TestRTCSecret(t *testing.T) {
	t.Setenv("HUDDLE_RTC_SECRET", "")
	a, err := rtcSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := rtcSecret()
	if len(a) != 32 || string(a) == string(b) {
		t.Fatalf("random secrets: %x, %x", a, b)
	}
	t.Setenv("HUDDLE_RTC_SECRET", "a-secret-of-at-least-32-characters!")
	if s, err := rtcSecret(); err != nil || string(s) != "a-secret-of-at-least-32-characters!" {
		t.Fatalf("set: %q, %v", s, err)
	}
}
