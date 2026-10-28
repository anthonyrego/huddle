package macvideo

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/video"
)

func TestBridge(t *testing.T) {
	if err := load(); err != nil {
		t.Fatal(err)
	}
}

// roundTrip encodes n pattern frames at w×h, forcing a keyframe at force
// (-1: none), and returns what came out, in order.
func roundTrip(t *testing.T, w, h, n, force int, cfg EncoderConfig) ([]Encoded, *Encoder) {
	t.Helper()
	var mu sync.Mutex
	var out []Encoded
	cfg.W, cfg.H = w, h
	enc, err := NewEncoder(cfg, func(e Encoded) {
		mu.Lock()
		out = append(out, e)
		mu.Unlock()
	})
	if st, ok := err.(*OSStatusError); ok && st.Status == couldNotFindEncoder {
		t.Skip("no H.264 encoder here (a CI virtual machine)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	t.Logf("%dx%d: low latency %v, warnings %v", w, h, enc.LowLatency, enc.Warnings)
	f := video.NewFrame(w, h)
	for i := range n {
		video.Draw(&f, uint32(i), false)
		b, err := enc.NewBuffer()
		if err != nil {
			t.Fatal(err)
		}
		if err := b.CopyFrom(f); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(b, time.Duration(i)*time.Second/30, i == force); err != nil {
			t.Fatal(err)
		}
		b.Release()
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, enc
}

func TestRoundTrip(t *testing.T) {
	const n, force = 60, 40
	out, enc := roundTrip(t, 640, 360, n, force, EncoderConfig{Bitrate: 800_000})
	if len(out) != n || enc.Dropped() != 0 {
		t.Fatalf("%d of %d frames out, %d dropped", len(out), n, enc.Dropped())
	}
	if !out[0].Key || !video.IsKeyframe(out[0].Data) {
		t.Fatal("the first frame isn't a keyframe")
	}
	if !out[force].Key || !video.IsKeyframe(out[force].Data) {
		t.Fatalf("frame %d was asked to be a keyframe and isn't", force)
	}
	keys := 0
	var bytes int
	for _, e := range out {
		if e.Key {
			keys++
		}
		bytes += len(e.Data)
	}
	t.Logf("%d keyframes, %.0f kbps", keys, float64(bytes*8)/2/1000)

	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var got, want video.Frame
	want = video.NewFrame(640, 360)
	var worst float64 = 1e9
	for i, e := range out {
		ok, err := dec.Decode(e.Data, &got)
		if err != nil || !ok {
			t.Fatalf("frame %d: decoded %v, %v", i, ok, err)
		}
		code, ok := video.ReadCode(got)
		if !ok || code != uint32(i) {
			t.Fatalf("frame %d reads as %d (%v)", i, code, ok)
		}
		video.Draw(&want, uint32(i), false)
		y, _ := video.PSNR(want, got)
		worst = min(worst, y)
	}
	t.Logf("worst brightness PSNR %.1f dB", worst)
	if worst < 30 {
		t.Fatalf("PSNR %.1f dB", worst)
	}
	if w, h := dec.Size(); w != 640 || h != 360 {
		t.Fatalf("decoder size %dx%d", w, h)
	}

	// A smaller stream into the same decoder: a simulcast layer switch.
	small, _ := roundTrip(t, 320, 180, 3, -1, EncoderConfig{Bitrate: 200_000})
	ok, err := dec.Decode(small[0].Data, &got)
	if err != nil || !ok || got.W != 320 || got.H != 180 {
		t.Fatalf("after the switch: %v %v, %dx%d", ok, err, got.W, got.H)
	}
	if code, ok := video.ReadCode(got); !ok || code != 0 {
		t.Fatalf("after the switch reads %d (%v)", code, ok)
	}
	t.Logf("rebuilds after a size change: %d", dec.Rebuilds)
}

func TestScale(t *testing.T) {
	src, err := NewBuffer(1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Release()
	f := video.NewFrame(1280, 720)
	video.Draw(&f, 77, true)
	if err := src.CopyFrom(f); err != nil {
		t.Fatal(err)
	}
	dst, err := NewBuffer(640, 360)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Release()
	s, err := NewScaler()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Scale(src, dst); err != nil {
		t.Fatal(err)
	}
	var got video.Frame
	if err := dst.CopyTo(&got); err != nil {
		t.Fatal(err)
	}
	if code, ok := video.ReadCode(got); !ok || code != 77 || !video.Flash(got) {
		t.Fatalf("scaled reads %d (%v), flash %v", code, ok, video.Flash(got))
	}
}
