package pcm

import "testing"

func TestOnsets(t *testing.T) {
	s := ClickTrain(5, 4800, 24000, 48000)
	at := Onsets(s, 0.01, 4800, 48000)
	if len(at) != 5 {
		t.Fatalf("found %d onsets, want 5: %v", len(at), at)
	}
	for i, a := range at {
		if want := 4800 + i*24000; a < want || a > want+48 {
			t.Fatalf("onset %d at %d, want just after %d", i, a, want)
		}
	}
}
