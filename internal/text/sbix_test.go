package text

import (
	"math"
	"os"
	"runtime"
	"testing"

	"github.com/go-text/typesetting/font"
)

const appleEmojiPath = "/System/Library/Fonts/Apple Color Emoji.ttc"

func appleEmoji(t *testing.T) (*font.Font, *sbixTable) {
	t.Helper()
	if _, err := os.Stat(appleEmojiPath); err != nil {
		t.Skip("no Apple Color Emoji on this system")
	}
	ft, sb, err := loadFace(font.FontID{File: appleEmojiPath})
	if err != nil {
		t.Fatal(err)
	}
	if sb == nil {
		t.Fatal("no sbix table found")
	}
	return ft, sb
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestSbixStaysOnDisk loads Apple Color Emoji (188 MB, nearly all sbix) and
// checks the bitmaps were not read: go-text never sees the table, and the
// heap grows by the text tables only.
func TestSbixStaysOnDisk(t *testing.T) {
	before := heapInUse()
	ft, sb := appleEmoji(t)
	grew := int64(heapInUse()) - int64(before)
	t.Logf("face loaded: heap +%.1f MB", float64(grew)/(1<<20))
	if grew > 40<<20 {
		t.Errorf("loading the face grew the heap by %d MB; the sbix table must stay on disk", grew>>20)
	}
	face := font.NewFace(ft)
	gid, ok := face.NominalGlyph('✅')
	if !ok {
		t.Fatal("no glyph for ✅")
	}
	if _, ok := face.GlyphDataBitmap(gid); ok {
		t.Error("go-text loaded the sbix bitmaps")
	}
	if _, ok := sb.glyph(uint32(gid), 20); !ok {
		t.Error("no sbix bitmap for ✅")
	}
	runtime.KeepAlive(ft)
}

// TestSbixBitmaps rasterizes ✅ at several sizes: the strike is the
// smallest at least as large, the result is premultiplied RGBA of the
// requested size, and the image's bottom sits on the baseline.
func TestSbixBitmaps(t *testing.T) {
	ft, sb := appleEmoji(t)
	gid, _ := font.NewFace(ft).NominalGlyph('✅')
	for _, px := range []float32{11, 16.25, 20, 26, 32.5, 40, 64, 200} {
		g, ok := sb.glyph(uint32(gid), px)
		if !ok {
			t.Fatalf("%vpx: no bitmap", px)
		}
		if g.ppem < int(math.Ceil(float64(px))) && g.ppem != 160 {
			t.Errorf("%vpx: took the %d ppem strike, want one at least as large", px, g.ppem)
		}
		var b Bitmap
		if !bitmapGlyph(g.png, g.ppem, g.originX, g.originY, px, &b) {
			t.Fatalf("%vpx: decode failed", px)
		}
		want := int(math.Round(float64(px)))
		if !b.Color || abs(b.W-want) > 1 || abs(b.H-want) > 1 {
			t.Errorf("%vpx: bitmap %d×%d color=%v, want about %d square", px, b.W, b.H, b.Color, want)
		}
		if b.Top != -b.H || b.Left != 0 {
			t.Errorf("%vpx: placed at (%d, %d), want the bottom-left on the pen", px, b.Left, b.Top)
		}
		opaque := 0
		for i := 0; i < len(b.Pix); i += 4 {
			a := b.Pix[i+3]
			if b.Pix[i] > a || b.Pix[i+1] > a || b.Pix[i+2] > a {
				t.Fatalf("%vpx: pixel %d is not premultiplied: %v", px, i/4, b.Pix[i:i+4])
			}
			if a == 255 {
				opaque++
			}
		}
		if opaque < len(b.Pix)/4/2 {
			t.Errorf("%vpx: only %d opaque pixels; ✅ is a filled square", px, opaque)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
