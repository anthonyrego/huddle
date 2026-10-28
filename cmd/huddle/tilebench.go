package main

// tiles-bench: what video tiles cost the window's main thread. Each frame
// does what the window does with its tiles: takes each tile's newest frame
// from its slot (a copy, under the slot's lock), puts it to the renderer,
// and records Prepare's upload and conversion, then submits. That time on
// this thread is the number; the GPU's part is timed apart, from the
// submit to its fence. The frames are the test pattern, drawn ahead (a
// decoder's work, on its own goroutine, isn't the window's). -check
// compares the GPU's pictures with video.Frame.RGBA (the same BT.709 sums
// on the CPU): whole, mirrored, and cropped to 16:9.

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"math"
	"os"
	"slices"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/gfx"
	"github.com/anthonyrego/huddle/internal/video"
)

func init() {
	register(&command{name: "tiles-bench", summary: "time converting and uploading video tiles on the main thread (offscreen, the test pattern)", run: runTilesBench})
}

func runTilesBench(args []string) error {
	fs := flag.NewFlagSet("tiles-bench", flag.ContinueOnError)
	tiles := fs.Int("tiles", 6, "tiles a frame")
	w := fs.Int("w", 640, "each tile's frame width")
	h := fs.Int("h", 360, "each tile's frame height")
	frames := fs.Int("frames", 300, "frames timed (after 30 to warm up)")
	check := fs.Bool("check", true, "compare the GPU's conversion with the CPU's")
	out := fs.String("out", "", "with -check: write the pictures compared here (a directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *w%2 != 0 || *h%2 != 0 || *tiles < 1 {
		return errors.New("-w and -h must be even, -tiles at least 1")
	}
	o, done, err := newOffscreen(false)
	if err != nil {
		return err
	}
	defer done()
	if *check {
		if err := checkConversion(o, *out); err != nil {
			return err
		}
	}

	// Eight frames a tile, each tile's numbers apart, so every frame is new.
	const kinds = 8
	pre := make([][]video.Frame, *tiles)
	slots := make([]*video.Latest, *tiles)
	for t := range pre {
		for k := range kinds {
			f := video.NewFrame(*w, *h)
			video.Draw(&f, uint32(t*1000+k), false)
			pre[t] = append(pre[t], f)
		}
		slots[t] = &video.Latest{}
	}
	seqs := make([]uint64, *tiles)
	r := o.renderer
	var empty gfx.Batch
	empty.Reset(64, 64)
	const warm = 30
	var cpu, gpu []time.Duration
	for i := range warm + *frames {
		for t, s := range slots {
			s.Put(pre[t][i%kinds]) // the decoder's part: not timed
		}
		start := time.Now()
		cmd, err := o.device.AcquireCommandBuffer()
		if err != nil {
			return err
		}
		for t, s := range slots {
			id := draw.ImageID(t + 1)
			seqs[t], _ = s.Take(seqs[t], func(f video.Frame) { r.PutFrame(id, f, 16.0/9) })
		}
		if err := r.Prepare(cmd, &empty); err != nil {
			return err
		}
		if err := r.VideoErr(); err != nil {
			return err
		}
		fence, err := cmd.SubmitAndAcquireFence()
		if err != nil {
			return err
		}
		took := time.Since(start)
		if err := o.device.WaitForFences(true, []*sdl.GPUFence{fence}); err != nil {
			return err
		}
		all := time.Since(start)
		o.device.ReleaseFence(fence)
		if i >= warm {
			cpu = append(cpu, took)
			gpu = append(gpu, all-took)
		}
	}
	mb := float64(*tiles**w**h*3/2) / 1e6
	fmt.Printf("%d tiles of %dx%d NV12 (%.2f MB a frame), %d frames\n", *tiles, *w, *h, mb, *frames)
	fmt.Printf("main thread (take, put, upload, convert, submit): median %s, p90 %s, max %s\n", tilePct(cpu, 50), tilePct(cpu, 90), tilePct(cpu, 100))
	fmt.Printf("GPU (submit to fence): median %s, p90 %s\n", tilePct(gpu, 50), tilePct(gpu, 90))
	return nil
}

// tilePct is the p-th percentile of ds, rounded to a microsecond.
func tilePct(ds []time.Duration, p int) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	i := min((len(s)-1)*p/100, len(s)-1)
	return s[i].Round(time.Microsecond)
}

// checkConversion converts pattern frames on the GPU, draws each picture
// at its own size, reads it back and compares it with the CPU's.
func checkConversion(o *offscreen, dir string) error {
	cases := []struct {
		name   string
		w, h   int
		mirror bool
		aspect float32
	}{
		{"whole", 640, 360, false, 0},
		{"mirrored", 640, 360, true, 0},
		{"cropped 4:3 to 16:9", 640, 480, false, 16.0 / 9},
	}
	for i, c := range cases {
		f := video.NewFrame(c.w, c.h)
		video.Draw(&f, uint32(4242+i), true)
		f.Mirror = c.mirror
		id := draw.ImageID(100 + i)
		o.renderer.PutFrame(id, f, c.aspect)
		want := f.RGBA()
		if c.mirror {
			want = mirrorRGBA(want)
		}
		if c.aspect > 0 {
			ch := int(float32(c.w)/c.aspect + 0.5)
			want = cropRGBA(want, image.Rect(0, (c.h-ch)/2, c.w, (c.h-ch)/2+ch))
		}
		pw, ph := want.Rect.Dx(), want.Rect.Dy()
		var l draw.List
		l.Image(draw.Rect{W: float32(pw), H: float32(ph)}, id, draw.Radii{}, 255)
		b := gfx.Batch{Glyphs: o.glyphs}
		b.Reset(pw, ph)
		b.AddList(&l, 1)
		got, err := gfx.RenderImage(o.device, o.renderer, pw, ph, draw.Color{A: 255}, &b)
		if err != nil {
			return err
		}
		if err := o.renderer.VideoErr(); err != nil {
			return err
		}
		o.renderer.DropFrame(id)
		p, worst := comparePSNR(got, want)
		fmt.Printf("check %s (%dx%d): PSNR %.1f dB against the CPU, largest difference %d of 255\n", c.name, pw, ph, p, worst)
		if dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := writePNG(fmt.Sprintf("%s/check-%d-gpu.png", dir, i), got); err != nil {
				return err
			}
			if err := writePNG(fmt.Sprintf("%s/check-%d-cpu.png", dir, i), want); err != nil {
				return err
			}
		}
		if p < 35 {
			return fmt.Errorf("check %s: PSNR %.1f dB is too low: the GPU's conversion isn't the CPU's", c.name, p)
		}
	}
	return nil
}

func mirrorRGBA(src *image.RGBA) *image.RGBA {
	b := src.Rect
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := range b.Dy() {
		for x := range b.Dx() {
			out.SetRGBA(x, y, src.RGBAAt(b.Min.X+b.Dx()-1-x, b.Min.Y+y))
		}
	}
	return out
}

func cropRGBA(src *image.RGBA, r image.Rectangle) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := range r.Dy() {
		for x := range r.Dx() {
			out.SetRGBA(x, y, src.RGBAAt(r.Min.X+x, r.Min.Y+y))
		}
	}
	return out
}

// comparePSNR is the PSNR of a against b over RGB, and the largest
// difference in any channel.
func comparePSNR(a, b *image.RGBA) (float64, int) {
	var sum float64
	worst, n := 0, 0
	for y := range b.Rect.Dy() {
		for x := range b.Rect.Dx() {
			pa, pb := a.RGBAAt(x, y), b.RGBAAt(x, y)
			for _, d := range []int{int(pa.R) - int(pb.R), int(pa.G) - int(pb.G), int(pa.B) - int(pb.B)} {
				sum += float64(d * d)
				worst = max(worst, d, -d)
				n++
			}
		}
	}
	if sum == 0 {
		return math.Inf(1), 0
	}
	return 10 * math.Log10(255*255/(sum/float64(n))), worst
}
