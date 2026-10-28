//go:build darwin

package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/anthonyrego/huddle/internal/platform/macvideo"
	"github.com/anthonyrego/huddle/internal/video"
)

// The forwarder on real layers: VideoToolbox encodes the test pattern at
// 720p, 360p and 180p (each encoder handed the 720p picture, as the app's
// sender does), keyframes come when the forwarder asks, and what one viewer
// gets across switches decodes, every picture showing the frame number its
// timestamp says.
func TestForwarderOnEncodedLayers(t *testing.T) {
	sizes := [][2]int{{1280, 720}, {640, 360}, {320, 180}}
	rates := []int{1_700_000, 450_000, 160_000}
	const frames = 150

	var mu sync.Mutex
	aus := make([][]macvideo.Encoded, 3)
	encs := make([]*macvideo.Encoder, 3)
	for i, sz := range sizes {
		i := i
		enc, err := macvideo.NewEncoder(macvideo.EncoderConfig{W: sz[0], H: sz[1], FPS: 30, Bitrate: rates[i], KeyEvery: 5 * time.Second, NoLowLatency: true}, func(e macvideo.Encoded) {
			mu.Lock()
			aus[i] = append(aus[i], e)
			mu.Unlock()
		})
		if st, ok := err.(*macvideo.OSStatusError); ok && st.Status == -12908 {
			t.Skip("no H.264 encoder here (a CI virtual machine)")
		}
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		encs[i] = enc
	}
	buf, err := encs[0].NewBuffer()
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Release()
	pic := video.NewFrame(1280, 720)

	f := newForwarder(1000)
	type got struct {
		seq     uint16
		h       rtp.Header
		payload []byte
	}
	var out []got
	emit := func(seq uint16, h *rtp.Header, payload []byte) {
		out = append(out, got{seq, *h, append([]byte(nil), payload...)})
	}
	targets := map[int]int{0: 2, 30: 0, 60: 1, 90: 2, 120: 0}
	var askKey [3]bool
	var seq [3]uint16
	pay := &codecs.H264Payloader{}
	t0 := time.Unix(0, 0)
	for n := range frames {
		if l, ok := targets[n]; ok && f.setTarget(l) {
			askKey[l] = true
		}
		video.Draw(&pic, uint32(n), false)
		if err := buf.CopyFrom(pic); err != nil {
			t.Fatal(err)
		}
		pts := time.Duration(n) * time.Second / 30
		for i, enc := range encs {
			if err := enc.Encode(buf, pts, askKey[i]); err != nil {
				t.Fatal(err)
			}
			askKey[i] = false
			if err := enc.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		now := t0.Add(pts)
		for layer := 2; layer >= 0; layer-- {
			mu.Lock()
			e := aus[layer][len(aus[layer])-1]
			mu.Unlock()
			payloads := pay.Payload(1200-48, e.Data)
			for j, p := range payloads {
				seq[layer]++
				h := rtp.Header{SequenceNumber: seq[layer], Timestamp: uint32(n) * 3000, Marker: j == len(payloads)-1}
				if f.push(layer, &h, p, now, emit) {
					askKey[layer] = true
				}
			}
		}
	}

	dec, err := macvideo.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	depay := &codecs.H264Packet{}
	var au []byte
	shown, decoded, lastShown := 0, 0, -1
	sizesSeen := map[[2]int]int{}
	pictures := video.NewFrame(1280, 720)
	for i, g := range out {
		if i > 0 && g.seq != out[i-1].seq+1 {
			t.Fatalf("packet %d: sequence %d after %d", i, g.seq, out[i-1].seq)
		}
		nal, err := depay.Unmarshal(g.payload)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		au = append(au, nal...)
		if !g.h.Marker {
			continue
		}
		ok, err := dec.Decode(au, &pictures)
		au = au[:0]
		if err != nil {
			t.Fatalf("frame at %d: %v", g.h.Timestamp/3000, err)
		}
		if !ok {
			t.Fatalf("frame at %d: no picture", g.h.Timestamp/3000)
		}
		decoded++
		w, h := dec.Size()
		sizesSeen[[2]int{w, h}]++
		want := int(g.h.Timestamp / 3000)
		if want <= lastShown {
			continue // a frame again at the new size: not shown
		}
		code, read := video.ReadCode(pictures)
		if !read || int(code) != want {
			t.Errorf("frame %d at %dx%d read as %d (%v)", want, w, h, code, read)
		}
		shown++
		lastShown = want
	}
	t.Logf("%d packets, %d pictures decoded, %d shown, sizes %v, stepped back %d", len(out), decoded, shown, sizesSeen, f.backs)
	if shown < frames-5 || len(sizesSeen) != 3 {
		t.Errorf("shown %d of %d frames, sizes %v", shown, frames, sizesSeen)
	}
}
