package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire"
)

type rooms map[string]bool

func (r rooms) EnsureRoom(ctx context.Context, room string) error { r[room] = true; return nil }

// join posts a dev join to srv and decodes the answer.
func join(t *testing.T, srv *httptest.Server, body string) wire.Joined {
	t.Helper()
	res, err := http.Post(srv.URL+"/dev/join", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var j wire.Joined
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	return j
}

var minter = rtctoken.Minter{Secret: []byte("a-secret-of-at-least-32-characters!")}

func TestDevJoin(t *testing.T) {
	rs := rooms{}
	rtc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("rtc")) })
	s := &Server{Rooms: rs, Minter: minter, RTC: rtc, Dev: true}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	j := join(t, srv, `{"room":"Design Review!","name":"Sam"}`)
	host := strings.TrimPrefix(srv.URL, "http://")
	if j.Room != "design-review" || !strings.HasPrefix(j.Identity, "sam-") || j.Media != wire.MediaBuiltin || j.MediaURL != "ws://"+host+"/rtc" || j.WSURL != "ws://"+host+"/ws" {
		t.Fatalf("joined %+v", j)
	}
	if strings.HasSuffix(j.Identity, wire.BotSuffix) {
		t.Fatalf("a person joined as a bot: %q", j.Identity)
	}
	if !rs["design-review"] {
		t.Fatal("room wasn't made")
	}
	g, err := minter.Verify(j.Token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g.Room != "design-review" || g.Identity != j.Identity || g.Name != "Sam" || g.Agent {
		t.Fatalf("grant %+v", g)
	}
	// The SFU's signalling is mounted.
	res, err := http.Get(srv.URL + "/rtc")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "rtc" {
		t.Fatalf("/rtc answered %d %q", res.StatusCode, b)
	}
}

func TestDevJoinBot(t *testing.T) {
	s := &Server{Rooms: rooms{}, Minter: minter, Dev: true}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	a := join(t, srv, `{"room":"r","name":"Echo","bot":true}`)
	b := join(t, srv, `{"room":"r","name":"Echo","bot":true}`)
	if !strings.HasPrefix(a.Identity, "echo-") || !strings.HasSuffix(a.Identity, wire.BotSuffix) {
		t.Fatalf("a bot joined as %q", a.Identity)
	}
	if a.Identity == b.Identity {
		t.Fatalf("two bots of one name share the identity %q", a.Identity)
	}
}

func TestNoRTCUnlessSet(t *testing.T) {
	srv := httptest.NewServer((&Server{Rooms: rooms{}}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/rtc")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/rtc with no SFU: %d", res.StatusCode)
	}
}

// A server with no secret to sign media tokens with refuses the join.
func TestDevJoinNoSecret(t *testing.T) {
	s := &Server{Rooms: rooms{}, Dev: true}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/dev/join", "application/json", strings.NewReader(`{"room":"r","name":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestNoDevJoinOutsideDev(t *testing.T) {
	s := &Server{Rooms: rooms{}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/dev/join", "application/json", strings.NewReader(`{"room":"r","name":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("dev join answered outside dev mode")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Design Review!": "design-review", "  a  b ": "a-b", "Ünïcode 2": "ünïcode-2", "!!": ""} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
