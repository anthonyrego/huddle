package sfu

// The SFU end to end on loopback: real sfuclient sessions through an
// in-process server. Voices are stand-in Opus frames, each naming who sent
// it and its number, with an RFC 6464 level.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// publishVoice publishes c's microphone and starts it speaking at level.
func publishVoice(t *testing.T, c *testClient, level uint8) (*speaker, string) {
	t.Helper()
	return publishSpeaker(t, c, level, false)
}

// publishSpeaker is publishVoice, the speaker clocked or not.
func publishSpeaker(t *testing.T, c *testClient, level uint8, clocked bool) (*speaker, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	tr, sid, err := c.s.PublishAudio(ctx, "microphone")
	if err != nil {
		t.Fatalf("%s: publish: %v", c.id, err)
	}
	return speak(t, c.id, tr, level, clocked), sid
}

// hearsAll waits until each client has heard a frame from each speaker but
// itself.
func hearsAll(t *testing.T, cs []*testClient, sps []*speaker) {
	t.Helper()
	waitFor(t, wait, "everyone to hear everyone", func() bool {
		for _, c := range cs {
			for _, sp := range sps {
				if sp.who != c.id && len(c.framesFrom(sp.who)) == 0 {
					return false
				}
			}
		}
		return true
	})
}

// allHeard stops the speakers and waits until each client has heard each
// one's last frame; it returns how many each sent.
func allHeard(t *testing.T, cs []*testClient, sps []*speaker) map[string]int {
	t.Helper()
	sent := map[string]int{}
	for _, sp := range sps {
		sp.halt()
		sent[sp.who] = int(sp.sent.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, c := range cs {
		for _, sp := range sps {
			if sp.who == c.id {
				continue
			}
			for time.Now().Before(deadline) {
				f := c.framesFrom(sp.who)
				if len(f) > 0 && f[len(f)-1] == sent[sp.who]-1 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	return sent
}

// before fails unless a comes before b in c's events.
func before(t *testing.T, c *testClient, a, b string) {
	t.Helper()
	ev := c.eventLog()
	i, j := slices.Index(ev, a), slices.Index(ev, b)
	if i < 0 || j < 0 || i > j {
		t.Errorf("%s: want %q before %q in %q", c.id, a, b, ev)
	}
}

func TestThreePeople(t *testing.T) {
	ts := newTestSFU(t, nil)
	start := time.Now()
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	cs := []*testClient{a, b, c}
	waitFor(t, wait, "joined and arrived", func() bool {
		return a.has("arrived b") && a.has("arrived c") && b.has("arrived c") && c.has("joined a,b")
	})
	if got := a.eventLog()[0]; got != "joined " {
		t.Errorf("a's first event %q, want it joined to no one", got)
	}
	if got := b.eventLog()[0]; got != "joined a" {
		t.Errorf("b's first event %q, want joined a", got)
	}

	var sps []*speaker
	sids := map[string]string{}
	for i, x := range cs {
		sp, sid := publishVoice(t, x, uint8(10+10*i))
		sps = append(sps, sp)
		sids[x.id] = sid
	}
	hearsAll(t, cs, sps)
	ready := time.Now()
	readyAt := map[string]int{}
	for _, sp := range sps {
		readyAt[sp.who] = int(sp.sent.Load())
	}
	time.Sleep(2 * time.Second)
	sent := allHeard(t, cs, sps)
	t.Logf("joined to everyone hearing everyone: %v; then 2 s", ready.Sub(start).Round(time.Millisecond))

	for _, x := range cs {
		for _, sp := range sps {
			if sp.who == x.id {
				continue
			}
			what := x.id + " hearing " + sp.who
			x.mu.Lock()
			h := x.fromLocked(sp.who, signal.Audio)
			frames, tss, levels, bad := slices.Clone(h.frames), slices.Clone(h.tss), slices.Clone(h.levels), slices.Clone(h.bad)
			x.mu.Unlock()
			checkRun(t, what, frames, readyAt[sp.who], sent[sp.who]-1)
			if len(bad) > 0 {
				t.Errorf("%s: %d packets that weren't frames: %q", what, len(bad), bad[:min(3, len(bad))])
			}
			for i := range frames {
				if i > 0 && tss[i]-tss[i-1] != uint32(frames[i]-frames[i-1])*960 {
					t.Errorf("%s: frame %d at RTP %d, %d after frame %d", what, frames[i], tss[i], tss[i]-tss[i-1], frames[i-1])
					break
				}
			}
			if want := byte(sp.level.Load()); len(levels) > 0 && levels[0] != want {
				t.Errorf("%s: audio level %#x, want %#x", what, levels[0], want)
			}
			t.Logf("%s: frames %d–%d, %d heard, none missing or repeated", what, frames[0], frames[len(frames)-1], len(frames))
			before(t, x, "info "+sp.who+" audio muted=false", "added "+sp.who+" audio")
		}
	}
	before(t, a, "arrived b", "info b audio muted=false")

	// c stops publishing: a and b hear the track go, and the renegotiation
	// takes it off their connections (its reads end).
	if err := c.s.Unpublish(sids["c"]); err != nil {
		t.Fatal(err)
	}
	for _, x := range []*testClient{a, b} {
		waitFor(t, wait, x.id+" to lose c's track", func() bool {
			x.mu.Lock()
			defer x.mu.Unlock()
			h := x.fromLocked("c", signal.Audio)
			return slices.Contains(x.events, "removed c audio") && h != nil && h.ended
		})
	}
	// Then c leaves.
	c.s.Close()
	for _, x := range []*testClient{a, b} {
		waitFor(t, wait, x.id+" to hear c leave", func() bool { return x.has("left c") })
		before(t, x, "removed c audio", "left c")
	}
	if strings.Contains(strings.Join(a.eventLog(), ";"), "disconnected") {
		t.Errorf("a disconnected: %q", a.eventLog())
	}
}
