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
		if req.Name == "Old" {
			json.NewEncoder(w).Encode(wire.Joined{Room: "standup", Identity: "old-1", Token: "t", Media: "livekit"})
			return
		}
		if r.URL.Path != "/dev/join" || req.Room != "standup" || req.Name != "Echo" || !req.Bot {
			http.Error(w, `{"message":"wrong join"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(wire.Joined{Room: "standup", Identity: "echo-1a2b-bot", Media: wire.MediaBuiltin, Token: "t"})
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
	// A server from before huddled had its own media server.
	_, err = Join(context.Background(), srv.URL, wire.DevJoin{Room: "standup", Name: "Old", Bot: true})
	if err == nil || err.Error() != `join: the server's media server is "livekit", not huddled's own: update the server` {
		t.Fatalf("an old server read as %v", err)
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
	j := wire.Joined{Room: "r", Identity: "ann-1", Media: wire.MediaBuiltin, MediaURL: "ws://h:8080/rtc", Token: "t", WSURL: "ws://h:8080/ws"}
	if got, want := RTC(j), (rtc.Join{URL: "ws://h:8080/rtc", Token: "t", Identity: "ann-1"}); got != want {
		t.Errorf("RTC(%+v) = %+v, want %+v", j, got, want)
	}
}
