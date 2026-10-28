package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthonyrego/huddle/internal/server/lktoken"
	"github.com/anthonyrego/huddle/internal/wire"
)

type rooms map[string]bool

func (r rooms) EnsureRoom(ctx context.Context, room string) error { r[room] = true; return nil }

func TestDevJoin(t *testing.T) {
	rs := rooms{}
	s := &Server{Rooms: rs, Minter: lktoken.Minter{Key: "devkey", Secret: "a-secret-of-at-least-32-characters!"}, LiveKitURL: "ws://lk:7880", Dev: true}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/dev/join", "application/json", strings.NewReader(`{"room":"Design Review!","name":"Sam"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var j wire.Joined
	json.NewDecoder(res.Body).Decode(&j)
	if j.Room != "design-review" || !strings.HasPrefix(j.Identity, "sam-") || j.Token == "" || j.LiveKitURL != "ws://lk:7880" || !strings.HasSuffix(j.WSURL, "/ws") {
		t.Fatalf("joined %+v", j)
	}
	if !rs["design-review"] {
		t.Fatal("room wasn't made")
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
