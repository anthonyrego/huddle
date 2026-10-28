package room

import (
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// countDecoder makes a frame of every unit.
type countDecoder struct{ n int }

func (d *countDecoder) Decode(_ []byte, f *video.Frame) (bool, error) {
	if f.W == 0 {
		*f = video.NewFrame(2, 2)
	}
	d.n++
	return true, nil
}

func (d *countDecoder) Close() {}

// TestPlayNewerOnly: a frame no newer than the last one shown is decoded
// (it may be the keyframe the decoder needs) but not shown, across the
// timestamp's wrap; one from far behind is a clock that started over, and
// is shown.
func TestPlayNewerOnly(t *testing.T) {
	var shown []uint32
	r := &room{tracks: map[string]*remote{}, cfg: &Connector{
		VideoFrame: func(_ rtc.VideoTrack, _ video.Frame, info VideoFrameInfo) { shown = append(shown, info.RTP) },
	}}
	dec := &countDecoder{}
	v := &videoTrack{r: r, dec: dec, aus: make(chan au, 8), subAt: time.Now()}
	a := uint32(0xfffff000)
	sent := []uint32{a, a + 3000, a + 1500, a + 6000, a + 6000, a + 6000 - 10*90000}
	for _, ts := range sent {
		v.aus <- au{data: []byte{0x65}, ts: ts}
	}
	close(v.aus)
	v.play()
	want := []uint32{sent[0], sent[1], sent[3], sent[5]}
	if len(shown) != len(want) {
		t.Fatalf("shown %x, want %x", shown, want)
	}
	for i := range want {
		if shown[i] != want[i] {
			t.Fatalf("shown %x, want %x", shown, want)
		}
	}
	if dec.n != len(sent) || v.Stats().Stale != 2 {
		t.Fatalf("%d decoded, %d stale", dec.n, v.Stats().Stale)
	}
}
