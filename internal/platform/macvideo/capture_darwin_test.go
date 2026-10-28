package macvideo

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ebitengine/purego/objc"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// TestObjC checks every selector, constant, protocol and method capture
// uses against the installed frameworks, without opening a device, asking
// for a permission or showing the picker: an unknown selector raises an
// exception that kills the process, so it's caught here instead.
func TestObjC(t *testing.T) {
	if err := loadObjC(); err != nil {
		t.Fatal(err)
	}
	responds := sel("respondsToSelector:")
	instancesRespond := sel("instancesRespondToSelector:")
	for cls, names := range selectors {
		name, classMethod := strings.CutPrefix(cls, "+")
		c := objc.GetClass(name)
		if c == 0 {
			t.Errorf("no class %s", name)
			continue
		}
		for _, s := range names {
			check := instancesRespond
			if classMethod {
				check = responds
			}
			if !objc.Send[bool](objc.ID(c), check, sel(s)) {
				t.Errorf("%s doesn't respond to %s", cls, s)
			}
		}
	}
	for _, names := range objcConstants {
		for _, name := range names {
			if constant(name) == 0 {
				t.Errorf("constant %s is nil", name)
			}
		}
	}
	if err := loadCamera(); err != nil {
		t.Fatalf("camera class: %v", err)
	}
	if err := loadScreen(); err != nil {
		t.Fatalf("screen classes: %v", err)
	}
	for cls, names := range protocols {
		c := objc.GetClass(cls)
		if c == 0 {
			t.Errorf("no class %s", cls)
			continue
		}
		for _, name := range names {
			p := objc.GetProtocol(name)
			if p == nil || !objc.Send[bool](objc.ID(c), sel("conformsToProtocol:"), p) {
				t.Errorf("%s doesn't conform to %s", cls, name)
			}
		}
	}
	for cls, sels := range classMethods {
		for _, s := range sels {
			if !objc.Send[bool](objc.ID(objc.GetClass(cls)), instancesRespond, sel(s)) {
				t.Errorf("%s doesn't respond to %s", cls, s)
			}
		}
	}
}

// TestTablesListed: every constant("…") and k("…") the package reads is in
// a table (both panic on one that isn't, which shows only when that path
// runs).
func TestTablesListed(t *testing.T) {
	listed := map[string]bool{}
	for _, m := range []map[string][]string{objcConstants, constants, addresses} {
		for _, names := range m {
			for _, n := range names {
				listed[n] = true
			}
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	use := regexp.MustCompile(`\b(?:constant|k)\("([A-Za-z0-9_]+)"\)`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range use.FindAllStringSubmatch(string(src), -1) {
			if !listed[m[1]] {
				t.Errorf("%s reads %s, which isn't in a table", f, m[1])
			}
		}
	}
}

// TestNotBundle: a test binary isn't Huddle.app, so it can't ask for the
// camera or open one; listing cameras opens nothing.
func TestNotBundle(t *testing.T) {
	if InBundle() {
		t.Fatal("a test binary reads as Huddle.app")
	}
	if err := RequestCameraAccess(func(bool) { t.Error("asked") }); err != ErrNotBundle {
		t.Fatalf("RequestCameraAccess = %v, want ErrNotBundle", err)
	}
	if _, err := StartCamera("", func(b *Buffer, _ time.Time) { b.Release(); t.Error("a frame") }, nil); err != ErrNotBundle {
		t.Fatalf("StartCamera = %v, want ErrNotBundle", err)
	}
	cams, err := Cameras()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cams {
		if c.Kind == CameraContinuity {
			t.Error("an iPhone listed outside the bundle")
		}
	}
	t.Logf("%d cameras listed; permission %v", len(cams), CameraAccess())
}

func TestShareSize(t *testing.T) {
	for _, c := range []struct {
		r     cgRect
		scale float64
		w, h  int
	}{
		{cgRect{W: 1512, H: 982}, 2, 1920, 1246},
		{cgRect{W: 1920, H: 1080}, 1, 1920, 1080},
		{cgRect{W: 801, H: 601}, 1, 800, 600},
		{cgRect{W: 20, H: 10}, 1, 64, 64},
	} {
		if w, h := shareSize(c.r, c.scale); w != c.w || h != c.h {
			t.Errorf("%v at %v: %dx%d, want %dx%d", c.r, c.scale, w, h, c.w, c.h)
		}
	}
}

func TestLayers(t *testing.T) {
	cam := CameraLayers(1280, 720)
	want := []rtc.Layer{{W: 1280, H: 720, Bitrate: 1_700_000}, {W: 640, H: 360, Bitrate: 450_000}, {W: 320, H: 180, Bitrate: 160_000}}
	for i := range want {
		if i >= len(cam) || cam[i] != want[i] {
			t.Fatalf("camera layers %v, want %v", cam, want)
		}
	}
	if l := CameraLayers(1920, 1080); l[0].W != 1280 || l[0].H != 720 {
		t.Fatalf("a 1080p camera's top layer %v", l[0])
	}
	if l := ScreenLayers(3024, 1964); len(l) != 2 || l[0].W != 1920 || l[1].W != 960 {
		t.Fatalf("screen layers %v", l)
	}
}

// TestSenderPattern sends the pattern through a three-layer sender and
// decodes every layer: each reads back its frame numbers, at its own size.
func TestSenderPattern(t *testing.T) {
	for _, scale := range []bool{false, true} {
		t.Run(map[bool]string{false: "vt-scales", true: "scaler"}[scale], func(t *testing.T) {
			s, err := NewSender(SenderConfig{Layers: CameraLayers(1280, 720), FPS: 30, Scale: scale})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			got := map[int][][]byte{}
			if err := s.Start(func(layer int, au []byte, _ time.Duration) {
				mu.Lock()
				got[layer] = append(got[layer], au)
				mu.Unlock()
			}); err != nil {
				if st, ok := err.(*OSStatusError); ok && st.Status == couldNotFindEncoder {
					t.Skip("no H.264 encoder here")
				}
				if strings.Contains(err.Error(), "-12908") {
					t.Skip("no H.264 encoder here")
				}
				t.Fatal(err)
			}
			pool, err := NewPool(1280, 720)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			start := time.Now()
			const n = 30
			for i := range n {
				b, err := pool.Get()
				if err != nil {
					t.Fatal(err)
				}
				b.Edit(func(f video.Frame) { video.Draw(&f, uint32(i), false) })
				s.Feed(b, start.Add(time.Duration(i)*time.Second/30))
				b.Release()
				if i == 10 {
					s.SetBitrates([]int{0, 450_000, 160_000}) // 720p paused
				}
				if i == 20 {
					s.SetBitrates([]int{1_700_000, 450_000, 160_000})
				}
			}
			s.Stop()
			sizes := []int{1280, 640, 320}
			for layer, aus := range got {
				dec, err := NewDecoder()
				if err != nil {
					t.Fatal(err)
				}
				var codes []int
				var f video.Frame
				for _, au := range aus {
					ok, err := dec.Decode(au, &f)
					if err != nil {
						t.Fatalf("layer %d: %v", layer, err)
					}
					if !ok {
						continue
					}
					if f.W != sizes[layer] {
						t.Fatalf("layer %d decodes %dx%d", layer, f.W, f.H)
					}
					c, ok := video.ReadCode(f)
					if !ok {
						t.Fatalf("layer %d: a frame without its code", layer)
					}
					codes = append(codes, int(c))
				}
				dec.Close()
				if !sort.IntsAreSorted(codes) {
					t.Fatalf("layer %d out of order: %v", layer, codes)
				}
				t.Logf("layer %d (%d wide): %d frames", layer, sizes[layer], len(codes))
				want := n
				if layer == 0 {
					want = n - 10 // paused for frames 11 to 20
				}
				if len(codes) != want {
					t.Fatalf("layer %d: %d frames, want %d: %v", layer, len(codes), want, codes)
				}
			}
			if len(got) != 3 {
				t.Fatalf("%d layers came out", len(got))
			}
		})
	}
}

// BenchmarkLayers measures three layers from one 720p picture, both ways:
// every encoder scaling the picture itself, or a Scaler first. Process CPU
// per frame is reported (VideoToolbox does part of its work in a service
// process of its own, not counted here).
func BenchmarkLayers(b *testing.B) {
	for _, scale := range []bool{false, true} {
		b.Run(map[bool]string{false: "vt-scales", true: "scaler"}[scale], func(b *testing.B) {
			s, err := NewSender(SenderConfig{Layers: CameraLayers(1280, 720), FPS: 30, Scale: scale})
			if err != nil {
				b.Fatal(err)
			}
			var n int
			var mu sync.Mutex
			if err := s.Start(func(int, []byte, time.Duration) { mu.Lock(); n++; mu.Unlock() }); err != nil {
				b.Skip(err)
			}
			pool, _ := NewPool(1280, 720)
			defer pool.Close()
			pics := make([]*Buffer, 8)
			for i := range pics {
				pics[i], _ = pool.Get()
				pics[i].Edit(func(f video.Frame) { video.Draw(&f, uint32(i), false) })
			}
			var ru0, ru1 syscall.Rusage
			syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
			start := time.Now()
			b.ResetTimer()
			for i := range b.N {
				s.Feed(pics[i%len(pics)], start.Add(time.Duration(i)*time.Second/30))
			}
			s.Stop()
			b.StopTimer()
			syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
			cpu := time.Duration(ru1.Utime.Nano()+ru1.Stime.Nano()-ru0.Utime.Nano()-ru0.Stime.Nano()) / time.Duration(b.N)
			b.ReportMetric(float64(cpu.Microseconds()), "cpu-µs/frame")
			for _, p := range pics {
				p.Release()
			}
		})
	}
}
