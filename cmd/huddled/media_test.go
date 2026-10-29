package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/wire"
)

func TestMediaLiveKit(t *testing.T) {
	t.Setenv("HUDDLE_MEDIA", "livekit")
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
	// The default, and no LiveKit needed: the SFU is huddled's, on the HTTP
	// port's number.
	t.Setenv("HUDDLE_MEDIA", "")
	t.Setenv("LIVEKIT_URL", "")
	t.Setenv("HUDDLE_RTC_SECRET", "")
	t.Setenv("HUDDLE_NODE_IP", "127.0.0.1")
	m, err := newMedia("127.0.0.1:0")
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	var s httpapi.Server
	m.api(&s)
	if b, ok := m.agent.(agent.Builtin); s.Media != wire.MediaBuiltin || s.RTC == nil || len(s.RTCMinter.Secret) != 32 || !ok || b.SFU != m.sfu {
		t.Fatalf("the API's media: %+v; the whiteboard's %#v", s, m.agent)
	}
	m.close()
	t.Setenv("HUDDLE_RTC_SECRET", "short")
	if _, err := newMedia("127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "HUDDLE_RTC_SECRET") {
		t.Fatalf("a short secret: %v", err)
	}
	t.Setenv("HUDDLE_NODE_IP", "not-an-ip")
	if _, err := newMedia("127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "HUDDLE_NODE_IP") {
		t.Fatalf("a node IP that isn't one: %v", err)
	}
	t.Setenv("HUDDLE_MEDIA", "carrier-pigeon")
	if _, err := newMedia(":8080"); err == nil || !strings.Contains(err.Error(), "builtin or livekit") {
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

// rooms lets every room in.
type rooms struct{}

func (rooms) EnsureRoom(context.Context, string) error { return nil }

// TestBuiltinThroughTheDevJoin is a builtin server end to end: two people
// join through the dev join, connect to the SFU at the URL and with the
// token it gave them, and one hears the other.
func TestBuiltinThroughTheDevJoin(t *testing.T) {
	t.Setenv("HUDDLE_MEDIA", "builtin")
	t.Setenv("HUDDLE_RTC_SECRET", "")
	t.Setenv("HUDDLE_NODE_IP", "127.0.0.1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := newMedia(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	api := &httpapi.Server{Rooms: rooms{}, Dev: true}
	m.api(api)
	srv := &http.Server{Handler: api.Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	server := "http://" + ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	join := func(name string, ev sfuclient.Events) *sfuclient.Session {
		j, err := devjoin.Join(ctx, server, wire.DevJoin{Room: "built-in", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if j.Media != wire.MediaBuiltin || !strings.HasSuffix(j.MediaURL, "/rtc") {
			t.Fatalf("joined %+v", j)
		}
		s, err := sfuclient.Join(ctx, j.MediaURL, j.Token, sfuclient.Options{Loopback: true}, ev)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	heard := make(chan int, 1)
	join("Bea", sfuclient.Events{TrackAdded: func(rt *sfuclient.RemoteTrack) {
		go func() {
			n := 0
			for {
				if _, _, err := rt.Track.ReadRTP(); err != nil {
					return
				}
				if n++; n == 25 {
					heard <- n
				}
			}
		}()
	}})
	ann := join("Ann", sfuclient.Events{})
	mic, _, err := ann.PublishAudio(ctx, "microphone")
	if err != nil {
		t.Fatal(err)
	}
	lvl := uint8(30)
	start := time.Now()
	for i := 0; ; i++ {
		select {
		case <-heard:
			t.Logf("25 frames heard %v after Ann began", time.Since(start).Round(time.Millisecond))
			return
		case <-ctx.Done():
			t.Fatal("Bea never heard 25 of Ann's frames")
		case <-time.After(time.Until(start.Add(time.Duration(i) * 20 * time.Millisecond))):
		}
		mic.WriteFrame([]byte{0xf8, 0xff, 0xfe}, uint32(i*960), &lvl)
	}
}
