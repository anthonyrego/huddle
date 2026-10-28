package devjoin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

func TestJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.DevJoin
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/dev/join" || req.Room != "standup" || req.Name != "Echo" || !req.Bot {
			http.Error(w, `{"message":"wrong join"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(wire.Joined{Room: "standup", Identity: "echo-1a2b-bot", Token: "t"})
	}))
	defer srv.Close()
	j, err := Join(context.Background(), srv.URL+"/", wire.DevJoin{Room: "standup", Name: "Echo", Bot: true})
	if err != nil || j.Identity != "echo-1a2b-bot" {
		t.Fatalf("joined %+v, %v", j, err)
	}
	_, err = Join(context.Background(), srv.URL, wire.DevJoin{Room: "elsewhere", Name: "Echo"})
	if err == nil || err.Error() != "join: 400 Bad Request: wrong join" {
		t.Fatalf("a refusal read as %v", err)
	}
}

func TestJoinUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = Join(context.Background(), "http://"+addr, wire.DevJoin{Room: "r", Name: "n"})
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("no answer read as %v", err)
	}
}

func TestRTC(t *testing.T) {
	for _, c := range []struct {
		j    wire.Joined
		want rtc.Join
	}{
		// A server from before Media: LiveKit at LiveKitURL.
		{wire.Joined{Identity: "ann-1", Token: "t", LiveKitURL: "ws://lk:7880"},
			rtc.Join{Kind: wire.MediaLiveKit, URL: "ws://lk:7880", Token: "t", Identity: "ann-1"}},
		{wire.Joined{Identity: "ann-1", Token: "t", Media: wire.MediaLiveKit, MediaURL: "ws://lk:7880", LiveKitURL: "ws://lk:7880"},
			rtc.Join{Kind: wire.MediaLiveKit, URL: "ws://lk:7880", Token: "t", Identity: "ann-1"}},
		{wire.Joined{Identity: "ann-1", Token: "t", Media: wire.MediaBuiltin, MediaURL: "ws://h:8080/rtc"},
			rtc.Join{Kind: wire.MediaBuiltin, URL: "ws://h:8080/rtc", Token: "t", Identity: "ann-1"}},
	} {
		if got := RTC(c.j); got != c.want {
			t.Errorf("RTC(%+v) = %+v, want %+v", c.j, got, c.want)
		}
	}
}
