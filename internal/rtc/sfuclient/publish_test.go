package sfuclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// lateServer is a stand-in SFU that answers each publish after a delay of
// its choosing, and keeps what it was told to unpublish.
type lateServer struct {
	delay func(n int) time.Duration

	mu          sync.Mutex
	published   map[string]string // SID by CID
	unpublished map[string]bool   // by SID
	errorAfter  map[int]bool      // publishes answered with an error first
}

func (ls *lateServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	var wmu sync.Mutex
	write := func(kind string, body any) {
		frame, _ := signal.Marshal(kind, body)
		wmu.Lock()
		defer wmu.Unlock()
		conn.Write(ctx, websocket.MessageText, frame)
	}
	if _, _, err := conn.Read(ctx); err != nil { // the join
		return
	}
	write(signal.KindJoined, signal.Joined{Room: "r", Self: signal.Participant{Identity: "a"}})
	n := 0
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var e signal.Envelope
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		switch e.Kind {
		case signal.KindPublish:
			var p signal.Publish
			e.Decode(&p)
			sid := fmt.Sprintf("TR_%d", n)
			d := ls.delay(n)
			withErr := ls.errorAfter[n]
			n++
			go func() {
				if withErr {
					// An error about something else fails every waiter;
					// the publish itself goes through.
					write(signal.KindError, signal.Error{Message: "settings: something else"})
				}
				time.Sleep(d)
				ls.mu.Lock()
				ls.published[p.CID] = sid
				ls.mu.Unlock()
				write(signal.KindPublished, signal.Published{CID: p.CID, SID: sid})
			}()
		case signal.KindUnpublish:
			var u signal.Unpublish
			e.Decode(&u)
			ls.mu.Lock()
			ls.unpublished[u.SID] = true
			ls.mu.Unlock()
		}
	}
}

// A publish given up on (its context done, the wait over, or failed by an
// error the server sent about something else) leaves no track on the
// server: a published that comes after, or between giving up and
// forgetting the publish, is unpublished. One that's taken in time isn't.
func TestPublishedLate(t *testing.T) {
	const timeout = 40 * time.Millisecond
	ls := &lateServer{
		published: map[string]string{}, unpublished: map[string]bool{},
		// Answers around the publish's deadline, some long after.
		delay:      func(int) time.Duration { return time.Duration(rand.Int64N(int64(2 * timeout))) },
		errorAfter: map[int]bool{3: true, 7: true},
	}
	// Half the publishes given up on wait before forgetting, so answers
	// come in the gap between the two.
	var gaps int
	gaveUp = func() {
		if gaps++; gaps%2 == 0 {
			time.Sleep(timeout)
		}
	}
	defer func() { gaveUp = func() {} }()
	hs := httptest.NewServer(ls)
	defer hs.Close()
	s, err := Join(context.Background(), "ws"+strings.TrimPrefix(hs.URL, "http"), "token", Options{Loopback: true, Log: slog.New(slog.DiscardHandler)}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const runs = 40
	results := map[string]string{} // the SID Publish returned, by CID ("" if it failed)
	for i := range runs {
		cid := fmt.Sprintf("cam-%d", i)
		tr, err := NewTrack(codecs.H264, cid, "a", "f", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		sid, err := s.Publish(ctx, signal.Publish{CID: cid, Kind: signal.Video, Source: signal.Camera,
			Layers: []signal.Layer{{RID: "f", W: 1280, H: 720}}}, tr)
		cancel()
		if err != nil {
			sid = ""
		}
		results[cid] = sid
	}

	// Every publish has been answered by now, and every unpublish sent.
	deadline := time.Now().Add(5 * time.Second)
	var failed, kept int
	for {
		ls.mu.Lock()
		bad := ""
		failed, kept = 0, 0
		for cid, got := range results {
			sid, answered := ls.published[cid]
			switch {
			case !answered:
				bad = cid + " not answered yet"
			case got == "" && !ls.unpublished[sid]:
				bad = fmt.Sprintf("%s (%s): given up on, and still published", cid, sid)
			case got != "" && got != sid:
				bad = fmt.Sprintf("%s: Publish returned %s, the server said %s", cid, got, sid)
			case got != "" && ls.unpublished[sid]:
				bad = fmt.Sprintf("%s (%s): published and taken, then unpublished", cid, sid)
			case got == "":
				failed++
			default:
				kept++
			}
		}
		ls.mu.Unlock()
		if bad == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(bad)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("%d publishes: %d taken, %d given up on and unpublished", runs, kept, failed)
	if failed == 0 || kept == 0 {
		t.Errorf("%d taken, %d given up on: the delays should give some of each", kept, failed)
	}
}
