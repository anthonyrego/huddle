package video

import (
	"bytes"
	"math"
	"testing"
)

func TestPatternCode(t *testing.T) {
	for _, size := range [][2]int{{1280, 720}, {640, 360}, {320, 180}} {
		f := NewFrame(size[0], size[1])
		for _, n := range []uint32{0, 1, 7, 1234567, 0xffffffff} {
			Draw(&f, n, n%2 == 1)
			got, ok := ReadCode(f)
			if !ok || got != n {
				t.Fatalf("%dx%d frame %d read as %d (%v)", size[0], size[1], n, got, ok)
			}
			if Flash(f) != (n%2 == 1) {
				t.Fatalf("%dx%d frame %d: flash read wrong", size[0], size[1], n)
			}
		}
	}
	// A frame without the pattern reads as nothing.
	f := NewFrame(640, 360)
	if _, ok := ReadCode(f); ok {
		t.Fatal("a black frame read as a code")
	}
}

func TestPSNR(t *testing.T) {
	a, b := NewFrame(64, 32), NewFrame(64, 32)
	Draw(&a, 3, false)
	b.CopyFrom(a)
	if y, uv := PSNR(a, b); !math.IsInf(y, 1) || !math.IsInf(uv, 1) {
		t.Fatalf("a copy: %v %v", y, uv)
	}
	for i := range b.Y {
		b.Y[i] ^= 1 // off by one everywhere: 48.1 dB
	}
	if y, _ := PSNR(a, b); math.Abs(y-48.13) > 0.01 {
		t.Fatalf("off by one: %.2f dB", y)
	}
}

func TestLatest(t *testing.T) {
	var l Latest
	if _, ok := l.Take(0, func(Frame) {}); ok {
		t.Fatal("an empty slot gave a frame")
	}
	f := NewFrame(4, 2)
	f.Y[0] = 9
	l.Put(f)
	f.Y[0] = 10 // the producer reuses its buffer
	l.Put(f)
	var got byte
	seq, ok := l.Take(0, func(g Frame) { got = g.Y[0] })
	if !ok || got != 10 || seq != 2 {
		t.Fatalf("took %d (seq %d, %v)", got, seq, ok)
	}
	if _, ok := l.Take(seq, func(Frame) {}); ok {
		t.Fatal("the same frame taken twice")
	}
}

func TestAnnexB(t *testing.T) {
	sps, pps, idr := []byte{0x67, 1, 2}, []byte{0x68, 3}, []byte{0x65, 0, 0, 0, 4} // zeros inside a unit stay
	au := AppendAnnexB(nil, sps, pps, idr)
	nals := SplitAnnexB(au)
	if len(nals) != 3 || !bytes.Equal(nals[2], idr) || NALType(nals[0]) != NALSPS {
		t.Fatalf("split %x", nals)
	}
	if !IsKeyframe(au) || IsKeyframe(AppendAnnexB(nil, []byte{0x41, 9})) {
		t.Fatal("keyframe test")
	}
	// 3-byte start codes too.
	if n := SplitAnnexB([]byte{0, 0, 1, 0x67, 5, 0, 0, 1, 0x68, 6}); len(n) != 2 || !bytes.Equal(n[0], []byte{0x67, 5}) {
		t.Fatalf("3-byte: %x", n)
	}
	back, err := AVCCToAnnexB(nil, AppendAVCC(nil, sps, pps, idr))
	if err != nil || !bytes.Equal(back, au) {
		t.Fatalf("avcc round trip: %x %v", back, err)
	}
	if _, err := AVCCToAnnexB(nil, []byte{0, 0, 0, 9, 1}); err == nil {
		t.Fatal("a short unit passed")
	}
}
