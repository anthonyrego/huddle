package text

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"sync"
	"sync/atomic"

	xdraw "golang.org/x/image/draw"
)

// sbixBytes counts sbix data read from disk, for FallbackStats.
var sbixBytes atomic.Int64

// sbixTable reads an sbix table (Apple's color bitmap glyphs) straight from
// the font file: the strike headers once, then each glyph's PNG when it is
// first drawn. Nothing else of the table is ever in memory.
type sbixTable struct {
	r      io.ReaderAt
	off    int64 // table offset in the file
	len    int64
	glyphs int // maxp.numGlyphs

	once    sync.Once
	strikes []sbixStrike // ascending ppem
	err     error
}

type sbixStrike struct {
	ppem int
	at   int64    // file offset of the strike
	offs []uint32 // glyphs+1 glyph data offsets, relative to the strike
}

func (t *sbixTable) load() error {
	t.once.Do(func() {
		var hdr [8]byte
		if _, err := t.r.ReadAt(hdr[:], t.off); err != nil {
			t.err = err
			return
		}
		n := int(binary.BigEndian.Uint32(hdr[4:]))
		if n <= 0 || n > 64 {
			t.err = fmt.Errorf("sbix: %d strikes", n)
			return
		}
		so := make([]byte, 4*n)
		if _, err := t.r.ReadAt(so, t.off+8); err != nil {
			t.err = err
			return
		}
		buf := make([]byte, 4+4*(t.glyphs+1))
		for i := range n {
			at := t.off + int64(binary.BigEndian.Uint32(so[4*i:]))
			if _, err := t.r.ReadAt(buf, at); err != nil {
				t.err = err
				return
			}
			sbixBytes.Add(int64(len(buf)))
			s := sbixStrike{ppem: int(binary.BigEndian.Uint16(buf)), at: at, offs: make([]uint32, t.glyphs+1)}
			for g := range s.offs {
				s.offs[g] = binary.BigEndian.Uint32(buf[4+4*g:])
			}
			t.strikes = append(t.strikes, s)
		}
		for i := 1; i < len(t.strikes); i++ { // ascending ppem
			for j := i; j > 0 && t.strikes[j].ppem < t.strikes[j-1].ppem; j-- {
				t.strikes[j], t.strikes[j-1] = t.strikes[j-1], t.strikes[j]
			}
		}
	})
	return t.err
}

// sbixGlyph is one glyph's bitmap record.
type sbixGlyph struct {
	ppem             int
	originX, originY int // bottom-left of the image relative to the glyph origin, strike pixels, y up
	png              []byte
}

// glyph returns the PNG for gid from the strike best suited to px pixels
// per em: the smallest at least that large (downscaling keeps detail),
// else the largest. Strikes without data for the glyph are skipped.
func (t *sbixTable) glyph(gid uint32, px float32) (sbixGlyph, bool) {
	if t.load() != nil || int(gid) >= t.glyphs {
		return sbixGlyph{}, false
	}
	first := len(t.strikes) - 1
	for i, s := range t.strikes {
		if float32(s.ppem) >= px {
			first = i
			break
		}
	}
	// Try the preferred strike, then larger ones, then smaller ones.
	order := make([]int, 0, len(t.strikes))
	for i := first; i < len(t.strikes); i++ {
		order = append(order, i)
	}
	for i := first - 1; i >= 0; i-- {
		order = append(order, i)
	}
	for _, i := range order {
		if g, ok := t.read(&t.strikes[i], gid, 0); ok {
			return g, true
		}
	}
	return sbixGlyph{}, false
}

func (t *sbixTable) read(s *sbixStrike, gid uint32, depth int) (sbixGlyph, bool) {
	start, end := s.offs[gid], s.offs[gid+1]
	if end <= start+8 || end-start > 8<<20 {
		return sbixGlyph{}, false
	}
	data := make([]byte, end-start)
	if _, err := t.r.ReadAt(data, s.at+int64(start)); err != nil {
		return sbixGlyph{}, false
	}
	sbixBytes.Add(int64(len(data)))
	g := sbixGlyph{
		ppem:    s.ppem,
		originX: int(int16(binary.BigEndian.Uint16(data))),
		originY: int(int16(binary.BigEndian.Uint16(data[2:]))),
	}
	switch string(data[4:8]) {
	case "png ":
		g.png = data[8:]
		return g, true
	case "dupe": // the same image as another glyph
		if depth == 0 && len(data) >= 10 {
			if dup := uint32(binary.BigEndian.Uint16(data[8:])); dup < uint32(t.glyphs) {
				return t.read(s, dup, 1)
			}
		}
	}
	return sbixGlyph{}, false
}

// bitmapGlyph scales a color bitmap glyph's PNG to px pixels per em, as
// premultiplied RGBA, and places it: (originX, originY) are the image's
// bottom-left corner relative to the pen in strike pixels, y up.
func bitmapGlyph(pngData []byte, ppem int, originX, originY int, px float32, dst *Bitmap) bool {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return false
	}
	b := img.Bounds()
	scale := float64(px) / float64(ppem)
	w := int(math.Round(float64(b.Dx()) * scale))
	h := int(math.Round(float64(b.Dy()) * scale))
	if w <= 0 || h <= 0 {
		*dst = Bitmap{Pix: dst.Pix[:0]}
		return true
	}
	n := w * h * 4
	pix := dst.Pix[:0]
	if cap(pix) < n {
		pix = make([]byte, n)
	}
	pix = pix[:n]
	out := &image.RGBA{Pix: pix, Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
	if w == b.Dx() && h == b.Dy() {
		// Same size: convert to premultiplied only.
		xdraw.Draw(out, out.Rect, img, b.Min, xdraw.Src)
	} else {
		// A tent filter widened to the scale factor: area-like averaging with
		// no negative lobes, so edges never ring into dark or light fringes.
		// Filtering happens on premultiplied values.
		xdraw.BiLinear.Scale(out, out.Rect, img, b, xdraw.Src, nil)
	}
	left := int(math.Round(float64(originX) * scale))
	bottom := int(math.Round(float64(originY) * scale))
	*dst = Bitmap{W: w, H: h, Left: left, Top: -(bottom + h), Color: true, Pix: pix}
	return true
}
