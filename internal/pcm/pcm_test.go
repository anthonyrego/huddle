package pcm

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func tone(n int, amp int16) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func TestDurationBytesRoundTrip(t *testing.T) {
	if got := Duration(48000, 24000); got != time.Second {
		t.Fatalf("Duration(48000, 24k) = %v, want 1s", got)
	}
	if got := Bytes(20*time.Millisecond, 16000); got != 640 {
		t.Fatalf("Bytes(20ms, 16k) = %d, want 640", got)
	}
	if got := Bytes(time.Duration(1), 16000); got != 0 {
		t.Fatalf("Bytes(1ns) = %d, want 0 (whole samples only)", got)
	}
}

func TestLevel(t *testing.T) {
	if got := Level(make([]byte, 640)); got != Silence {
		t.Fatalf("silence = %v, want %v", got, Silence)
	}
	if got := Level(nil); got != Silence {
		t.Fatalf("empty = %v, want %v", got, Silence)
	}
	// A square wave at half scale has RMS 0.5: about -6.02 dBFS.
	if got := Level(tone(320, 16384)); math.Abs(got-(-6.02)) > 0.05 {
		t.Fatalf("half-scale square = %.2f dBFS, want -6.02", got)
	}
}

func TestDetectorHangover(t *testing.T) {
	d := Detector{Threshold: -40, Hangover: 300 * time.Millisecond}
	t0 := time.Unix(0, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	if e, _ := d.Feed(-60, at(0)); e != None {
		t.Fatalf("quiet start gave %v", e)
	}
	if e, when := d.Feed(-20, at(20)); e != Start || !when.Equal(at(20)) {
		t.Fatalf("loud chunk gave %v at %v, want Start at 20ms", e, when)
	}
	d.Feed(-20, at(40))
	// A 200 ms pause between words doesn't end speech.
	if e, _ := d.Feed(-60, at(240)); e != None {
		t.Fatalf("short pause gave %v", e)
	}
	d.Feed(-20, at(260))
	if e, _ := d.Feed(-60, at(500)); e != None {
		t.Fatalf("240ms of quiet gave %v", e)
	}
	e, when := d.Feed(-60, at(560))
	if e != End || !when.Equal(at(260)) {
		t.Fatalf("300ms of quiet gave %v at %v, want End at the last loud chunk (260ms)", e, when)
	}
	if d.Speaking() {
		t.Fatal("still speaking after End")
	}
}
