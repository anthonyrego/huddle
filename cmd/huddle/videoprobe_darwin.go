package main

// video-probe: VideoToolbox through purego, end to end, with no camera and
// no screen. The test pattern is encoded at 720p in real time, decoded as
// it comes out and checked frame by frame (its barcode, its PSNR), with a
// keyframe asked for partway; then scaling, an encoder handed a frame of
// another size, and a decoder given a stream that changes size.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/platform/macvideo"
	"github.com/anthonyrego/huddle/internal/video"
)

func init() {
	register(&command{name: "video-probe", summary: "VideoToolbox end to end on the test pattern: encode, decode, scale (no camera, no screen)", run: runVideoProbe})
}

func runVideoProbe(args []string) error {
	fs := flag.NewFlagSet("video-probe", flag.ContinueOnError)
	w := fs.Int("width", 1280, "frame width")
	h := fs.Int("height", 720, "frame height")
	fps := fs.Int("fps", 30, "frames a second, paced in real time")
	dur := fs.Duration("for", 5*time.Second, "how long to encode")
	bitrate := fs.Int("bitrate", 1_700_000, "bits a second")
	forceAt := fs.Int("key-at", 90, "ask for a keyframe at this frame")
	out := fs.String("out", "", "a directory for PNGs of decoded frames")
	if err := fs.Parse(args); err != nil {
		return err
	}
	n := int(dur.Seconds() * float64(*fps))
	type got struct {
		e   macvideo.Encoded
		dec time.Duration
		ok  bool
		psY float64
		psC float64
	}
	results := make([]got, 0, n)
	encoded := make(chan macvideo.Encoded, n)
	enc, err := macvideo.NewEncoder(macvideo.EncoderConfig{W: *w, H: *h, FPS: *fps, Bitrate: *bitrate}, func(e macvideo.Encoded) { encoded <- e })
	if err != nil {
		return err
	}
	fmt.Printf("encoder %dx%d at %d fps, %d kbps: low-latency rate control %v", *w, *h, *fps, *bitrate/1000, enc.LowLatency)
	if len(enc.Warnings) > 0 {
		fmt.Printf(", turned down: %s", strings.Join(enc.Warnings, "; "))
	}
	fmt.Println()
	dec, err := macvideo.NewDecoder()
	if err != nil {
		return err
	}
	defer dec.Close()

	// Decode as frames come out, as a receiver would.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var f video.Frame
		want := video.NewFrame(*w, *h)
		for e := range encoded {
			t0 := time.Now()
			ok, err := dec.Decode(e.Data, &f)
			g := got{e: e, dec: time.Since(t0)}
			if err == nil && ok {
				code, read := video.ReadCode(f)
				g.ok = read && code == uint32(e.Seq-1)
				video.Draw(&want, uint32(e.Seq-1), false)
				g.psY, g.psC = video.PSNR(want, f)
				if *out != "" && (e.Seq == 1 || int(e.Seq-1) == *forceAt) {
					writePNG(filepath.Join(*out, fmt.Sprintf("decoded-%03d.png", e.Seq-1)), f.RGBA())
				}
			}
			results = append(results, g)
		}
	}()
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
	}
	f := video.NewFrame(*w, *h)
	tick := time.NewTicker(time.Second / time.Duration(*fps))
	start := time.Now()
	for i := range n {
		<-tick.C
		video.Draw(&f, uint32(i), false)
		b, err := enc.NewBuffer()
		if err != nil {
			return err
		}
		if err := b.CopyFrom(f); err != nil {
			return err
		}
		err = enc.Encode(b, time.Since(start), i == *forceAt)
		b.Release()
		if err != nil {
			return err
		}
	}
	tick.Stop()
	enc.Flush()
	enc.Close()
	close(encoded)
	wg.Wait()

	slices.SortFunc(results, func(a, b got) int { return int(a.e.Seq) - int(b.e.Seq) })
	var took, decs []time.Duration
	var psY, psC []float64
	var keys []int
	bytes, bad := 0, 0
	for _, g := range results {
		took, decs = append(took, g.e.Took), append(decs, g.dec)
		psY, psC = append(psY, g.psY), append(psC, g.psC)
		bytes += len(g.e.Data)
		if g.e.Key {
			keys = append(keys, int(g.e.Seq-1))
		}
		if !g.ok {
			bad++
		}
	}
	fmt.Printf("frames: %d in, %d out, %d dropped; %d not decoded or read wrong\n", n, len(results), enc.Dropped(), bad)
	fmt.Printf("keyframes at %v (asked at %d)\n", keys, *forceAt)
	fmt.Printf("bitrate: %.0f kbps (asked %d)\n", float64(bytes*8)/dur.Seconds()/1000, *bitrate/1000)
	fmt.Printf("encode (handed over to out): median %v, p90 %v, max %v\n", pct(took, 50), pct(took, 90), pct(took, 100))
	fmt.Printf("decode: median %v, p90 %v, max %v\n", pct(decs, 50), pct(decs, 90), pct(decs, 100))
	fmt.Printf("PSNR brightness: median %.1f dB, worst %.1f; colour: median %.1f, worst %.1f\n",
		pctF(psY, 50), pctF(psY, 0), pctF(psC, 50), pctF(psC, 0))
	var under []string
	for _, g := range results {
		if g.psY < 35 {
			under = append(under, fmt.Sprintf("%d (%.1f dB, %d bytes, key %v)", g.e.Seq-1, g.psY, len(g.e.Data), g.e.Key))
		}
	}
	if len(under) > 0 {
		fmt.Printf("under 35 dB: %s\n", strings.Join(under, ", "))
	}
	if !slices.Contains(keys, *forceAt) && *forceAt < n {
		fmt.Printf("FAIL: frame %d was asked to be a keyframe\n", *forceAt)
	}

	if err := probeScaling(*w, *h, *out); err != nil {
		fmt.Println("scaling:", err)
	}
	if err := probeLayerSwitch(*w, *h); err != nil {
		fmt.Println("layer switch:", err)
	}
	return nil
}

// probeScaling times a scale to half size, and hands an encoder a frame of
// another size to see what it does.
func probeScaling(w, h int, out string) error {
	src, err := macvideo.NewBuffer(w, h)
	if err != nil {
		return err
	}
	defer src.Release()
	f := video.NewFrame(w, h)
	video.Draw(&f, 4242, true)
	src.CopyFrom(f)
	dst, err := macvideo.NewBuffer(w/2, h/2)
	if err != nil {
		return err
	}
	defer dst.Release()
	s, err := macvideo.NewScaler()
	if err != nil {
		return err
	}
	defer s.Close()
	var times []time.Duration
	for range 20 {
		t0 := time.Now()
		if err := s.Scale(src, dst); err != nil {
			return err
		}
		times = append(times, time.Since(t0))
	}
	var g video.Frame
	dst.CopyTo(&g)
	code, ok := video.ReadCode(g)
	fmt.Printf("scale %dx%d to %dx%d: median %v; reads %d (%v), flash %v\n", w, h, w/2, h/2, pct(times, 50), code, ok, video.Flash(g))

	// A full-size frame into a half-size encoder.
	var mu sync.Mutex
	var au []byte
	enc, err := macvideo.NewEncoder(macvideo.EncoderConfig{W: w / 2, H: h / 2, Bitrate: 600_000}, func(e macvideo.Encoded) {
		mu.Lock()
		au = e.Data
		mu.Unlock()
	})
	if err != nil {
		return err
	}
	encErr := enc.Encode(src, 0, false)
	enc.Close()
	mu.Lock()
	defer mu.Unlock()
	switch {
	case encErr != nil:
		fmt.Printf("a %dx%d frame into a %dx%d encoder: refused (%v): lower layers scale first\n", w, h, w/2, h/2, encErr)
	case au == nil:
		fmt.Printf("a %dx%d frame into a %dx%d encoder: nothing came out: lower layers scale first\n", w, h, w/2, h/2)
	default:
		dec, err := macvideo.NewDecoder()
		if err != nil {
			return err
		}
		defer dec.Close()
		var g video.Frame
		ok, err := dec.Decode(au, &g)
		code, read := video.ReadCode(g)
		fmt.Printf("a %dx%d frame into a %dx%d encoder: came out %dx%d (decoded %v %v), reads %d (%v)\n", w, h, w/2, h/2, g.W, g.H, ok, err, code, read)
		if out != "" {
			writePNG(filepath.Join(out, "mismatched.png"), g.RGBA())
		}
	}
	return nil
}

// probeLayerSwitch gives one decoder a stream, then a stream of half the
// size: what a subscriber sees when the SFU moves it down a layer.
func probeLayerSwitch(w, h int) error {
	dec, err := macvideo.NewDecoder()
	if err != nil {
		return err
	}
	defer dec.Close()
	var g video.Frame
	for _, size := range [][2]int{{w, h}, {w / 2, h / 2}, {w, h}} {
		au, err := oneKeyframe(size[0], size[1], 7)
		if err != nil {
			return err
		}
		ok, err := dec.Decode(au, &g)
		if err != nil || !ok {
			return fmt.Errorf("%dx%d: %v %v", size[0], size[1], ok, err)
		}
		code, read := video.ReadCode(g)
		fmt.Printf("layer %dx%d: decoded %dx%d, reads %d (%v), sessions made again so far %d\n", size[0], size[1], g.W, g.H, code, read, dec.Rebuilds)
	}
	return nil
}

// oneKeyframe encodes pattern frame n at w×h.
func oneKeyframe(w, h int, n uint32) ([]byte, error) {
	var mu sync.Mutex
	var au []byte
	enc, err := macvideo.NewEncoder(macvideo.EncoderConfig{W: w, H: h, Bitrate: 1_000_000}, func(e macvideo.Encoded) {
		mu.Lock()
		au = e.Data
		mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	b, err := enc.NewBuffer()
	if err != nil {
		return nil, err
	}
	f := video.NewFrame(w, h)
	video.Draw(&f, n, false)
	b.CopyFrom(f)
	err = enc.Encode(b, 0, true)
	b.Release()
	enc.Close()
	mu.Lock()
	defer mu.Unlock()
	if err == nil && au == nil {
		err = errors.New("nothing came out")
	}
	return au, err
}

func pct(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)*p/100, len(s)-1)].Round(10 * time.Microsecond)
}

func pctF(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[min(len(s)*p/100, len(s)-1)]
}
