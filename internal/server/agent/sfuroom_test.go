package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/server/sfu"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// stamped is voice frame n as the tests write it: an Opus TOC byte (20 ms
// CELT, one frame) and n, so a reader can tell which frame it got and when
// it was written. No SFU decodes a voice.
func stamped(n uint32) []byte { return binary.BigEndian.AppendUint32([]byte{0xf8}, n) }

func unstamp(b []byte) (uint32, bool) {
	if len(b) != 5 || b[0] != 0xf8 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[1:]), true
}

// writes keeps when each stamped frame was written.
type writes struct {
	mu sync.Mutex
	at map[uint32]time.Time
}

func (w *writes) wrote(n uint32) {
	w.mu.Lock()
	if w.at == nil {
		w.at = map[uint32]time.Time{}
	}
	w.at[n] = time.Now()
	w.mu.Unlock()
}

func (w *writes) when(n uint32) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.at[n]
	return t, ok
}

// transit reads a voice until it ends, timing each stamped frame from its
// write to its read (got), and returns how the reads ended.
func transit(track VoiceTrack, w *writes, got func(time.Duration)) error {
	for {
		track.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		p, _, err := track.ReadRTP()
		now := time.Now()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			continue
		}
		if err != nil {
			return err
		}
		n, ok := unstamp(p.Payload)
		if !ok {
			return fmt.Errorf("a packet that isn't the voice's: %x", p.Payload)
		}
		if at, ok := w.when(n); ok {
			got(now.Sub(at))
		}
	}
}

// durations collects transits.
type durations struct {
	mu sync.Mutex
	ds []time.Duration
}

func (d *durations) add(x time.Duration) {
	d.mu.Lock()
	d.ds = append(d.ds, x)
	d.mu.Unlock()
}

func (d *durations) get() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ds)
}

func median(ds []time.Duration) (time.Duration, time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2], s[len(s)*95/100]
}

// TestAudioOnly: on huddled's own SFU, in process (CI runs it), someone
// publishes a voice and a camera; the whiteboard, joined through Builtin,
// is handed the voice and reads its packets, never the camera, counts one
// person, and is heard by them; someone leaving ends their voice.
func TestAudioOnly(t *testing.T) {
	tokens := rtctoken.Minter{Secret: []byte("a test secret")}
	log := slog.New(slog.DiscardHandler)
	s, err := sfu.New(sfu.Config{Network: sfu.Network{IP: "127.0.0.1", Loopback: true}, Tokens: tokens, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	defer func() { s.Close(); hs.Close() }()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/rtc"
	room := fmt.Sprintf("audio-only-%06d", rand.IntN(1e6))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Someone with a voice and a camera, who hears the whiteboard.
	tok, err := tokens.Mint(rtctoken.Grant{Room: room, Identity: "someone", Name: "Someone"})
	if err != nil {
		t.Fatal(err)
	}
	var wbWrites writes
	var wbTransit durations
	wbHeard := make(chan struct{})
	sess, err := sfuclient.Join(ctx, url, tok, sfuclient.Options{Loopback: true, Log: log}, sfuclient.Events{
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			if rt.Participant != Identity || rt.Kind != signal.Audio {
				return
			}
			go transit(rt.Track, &wbWrites, func(d time.Duration) {
				wbTransit.add(d)
				if len(wbTransit.get()) == 25 {
					close(wbHeard)
				}
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	voice, _, err := sess.PublishAudio(ctx, "microphone")
	if err != nil {
		t.Fatal(err)
	}
	var layers []*sfuclient.Track
	for _, rid := range []string{"q", "h", "f"} {
		tr, err := sfuclient.NewTrack(codecs.H264, "cam", "someone", rid, nil)
		if err != nil {
			t.Fatal(err)
		}
		layers = append(layers, tr)
	}
	if _, err := sess.Publish(ctx, signal.Publish{CID: "cam", Kind: signal.Video, Source: signal.Camera,
		Layers: []signal.Layer{{RID: "f", W: 1280, H: 720}, {RID: "h", W: 640, H: 360}, {RID: "q", W: 320, H: 180}}}, layers...); err != nil {
		t.Fatal(err)
	}
	var someoneWrites writes
	stop := make(chan struct{})
	talking := make(chan struct{})
	go func() {
		defer close(talking)
		lvl := uint8(30)
		start := time.Now()
		for n := uint32(0); ; n++ {
			select {
			case <-stop:
				return
			case <-time.After(time.Until(start.Add(time.Duration(n) * 20 * time.Millisecond))):
			}
			someoneWrites.wrote(n)
			voice.WriteFrame(stamped(n), n*960, &lvl)
			if n%2 == 0 {
				for _, l := range layers {
					l.WriteFrame([]byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00}, n*1500, nil)
				}
			}
		}
	}()

	// The whiteboard joins.
	var mu sync.Mutex
	var arrived, left, handed []string
	var someoneTransit durations
	ended := make(chan error, 1)
	mr, err := Builtin{SFU: s}.Join(ctx, room, Hearing{
		Arrived: func(identity, name string) { mu.Lock(); arrived = append(arrived, identity+" "+name); mu.Unlock() },
		Left:    func(identity string) { mu.Lock(); left = append(left, identity); mu.Unlock() },
		Voice: func(identity, _ string, track VoiceTrack) {
			mu.Lock()
			handed = append(handed, identity+" "+track.Codec().MimeType)
			mu.Unlock()
			go func() { ended <- transit(track, &someoneWrites, someoneTransit.add) }()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Leave()
	if n := mr.People(); n != 1 {
		t.Errorf("People: %d, want 1", n)
	}

	// The whiteboard speaks, as voiceOut does: a frame every 20 ms.
	wbStop := make(chan struct{})
	wbDone := make(chan struct{})
	go func() {
		defer close(wbDone)
		start := time.Now()
		for n := uint32(0); ; n++ {
			select {
			case <-wbStop:
				return
			case <-time.After(time.Until(start.Add(time.Duration(n) * 20 * time.Millisecond))):
			}
			wbWrites.wrote(n)
			mr.Publish(stamped(n), 12345+n*960)
		}
	}()
	select {
	case <-wbHeard:
	case <-ctx.Done():
		t.Fatal("someone never heard 25 of the whiteboard's frames")
	}
	close(wbStop)
	<-wbDone
	time.Sleep(2 * time.Second) // the voice keeps coming, the camera too

	mu.Lock()
	if len(handed) != 1 || handed[0] != "someone "+codecs.Opus.MimeType {
		t.Errorf("voices handed over: %q, want someone's alone", handed)
	}
	if len(arrived) != 1 || arrived[0] != "someone Someone" {
		t.Errorf("arrived %q", arrived)
	}
	mu.Unlock()

	// Someone leaves: their voice ends, and they're gone.
	close(stop)
	<-talking
	sess.Close()
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Errorf("someone's voice ended with %v, want io.EOF (and only voice packets)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("someone's voice didn't end when they left")
	}
	deadline := time.Now().Add(5 * time.Second)
	for mr.People() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if mr.People() != 0 || !slices.Equal(left, []string{"someone"}) {
		t.Errorf("after leaving: %d people, left %q", mr.People(), left)
	}
	ds := someoneTransit.get()
	if len(ds) < 100 {
		t.Fatalf("the whiteboard read %d of someone's frames", len(ds))
	}
	med, p95 := median(ds)
	t.Logf("someone → the whiteboard: %d frames, transit median %v, p95 %v", len(ds), med.Round(10*time.Microsecond), p95.Round(10*time.Microsecond))
	med, p95 = median(wbTransit.get())
	t.Logf("the whiteboard → someone: %d frames, transit median %v, p95 %v", len(wbTransit.get()), med.Round(10*time.Microsecond), p95.Round(10*time.Microsecond))
}
