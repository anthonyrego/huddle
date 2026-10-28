package audio

import (
	"math"
	"testing"
)

// tone is n samples of a sine at amplitude a.
func tone(n int, a float64) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(a * math.Sin(float64(i)*0.07))
	}
	return s
}

func TestLoudestFindsTheVoiceAndHolds(t *testing.T) {
	var p loudest
	silent, voice, quiet := make([]float32, 4410), tone(4410, 0.2), tone(4410, 0.14)
	// The mic on the second input: picked within half a second.
	got := -1
	for i := range 5 {
		if got = p.update([][]float32{silent, voice}); got == 1 {
			t.Logf("picked input 2 after %d buffers", i+1)
			break
		}
	}
	if got != 1 {
		t.Fatalf("the voice on input 2 wasn't picked: %d", got)
	}
	// Both inputs carrying it, the first 3 dB quieter: no flapping back.
	for range 50 {
		if got = p.update([][]float32{quiet, voice}); got != 1 {
			t.Fatal("moved to a channel that isn't 6 dB louder")
		}
	}
	// The voice moves to input 1 and input 2 goes quiet: it follows.
	for range 30 {
		got = p.update([][]float32{voice, silent})
	}
	if got != 0 {
		t.Fatalf("didn't follow the voice to input 1: %d", got)
	}
	// A different channel count starts over.
	if got = p.update([][]float32{voice}); got != 0 || len(p.power) != 1 {
		t.Fatalf("one channel: %d, %d powers", got, len(p.power))
	}
}
