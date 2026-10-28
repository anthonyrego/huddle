package audio

import (
	"os"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/platform"
)

// TestPlaybackDrains plays silence through the speakers and checks that a
// flush lets Queued reach 0. It needs an audio device, so it runs only with
// HUDDLE_AUDIO_TEST=1. It never opens the microphone.
func TestPlaybackDrains(t *testing.T) {
	if os.Getenv("HUDDLE_AUDIO_TEST") != "1" {
		t.Skip("set HUDDLE_AUDIO_TEST=1 to run (opens the speakers and plays silence)")
	}
	if err := platform.InitAudio(); err != nil {
		t.Fatal(err)
	}
	defer platform.Quit()
	p, err := OpenPlayback()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Put(make([]byte, pcm.Bytes(300*time.Millisecond, OutRate))); err != nil {
		t.Fatal(err)
	}

	// Wait until the device stops taking audio: what's left is what SDL holds.
	deadline := time.Now().Add(2 * time.Second)
	last, since := p.Queued(), time.Now()
	for time.Since(since) < 150*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatalf("playback still moving after 2s: %d bytes queued", p.Queued())
		}
		time.Sleep(10 * time.Millisecond)
		if q := p.Queued(); q != last {
			last, since = q, time.Now()
		}
	}
	dev, _, _ := p.s.Device().Format()
	t.Logf("device at %d Hz; %d bytes (%s) stay queued without a flush", dev.Freq, last, pcm.Duration(last, OutRate))

	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for p.Queued() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d bytes still queued 1s after the flush", p.Queued())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Log("queued reached 0 after the flush")
}
