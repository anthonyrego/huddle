package video

import "math"

// The test pattern: video's agent checks need no camera and no screen. It
// is a gradient with bars, both moving every frame so the encoder has
// motion and detail to work on; the frame number as a barcode along the
// top, read back after the picture has been through an encoder and a
// network; and a patch at the bottom right that is white during a flash,
// which lip-sync checks pair with a beep.

const (
	codeBits = 32
	cells    = 4 + codeBits + 4 // a 1010 guard, the number (high bit first), a check nibble
	black    = 16               // video range
	white    = 235
	neutral  = 128
)

// cellSize is a barcode cell's side for a frame w wide: even, and leaving
// two cells of margin either side.
func cellSize(w int) int { return max((w/(cells+4))&^1, 4) }

// Draw paints frame n of the pattern into f.
func Draw(f *Frame, n uint32, flash bool) {
	w, h := f.W, f.H
	shift := int(n)
	for y := range h {
		row := f.Y[y*f.YStride : y*f.YStride+w]
		ramp := y * 60 / h
		for x := range row {
			v := 16 + (x*160/w+ramp+shift*3)%220
			if (x+shift*4)%96 < 24 {
				v = 251 - v // a bar
			}
			row[x] = byte(v)
		}
	}
	for y := range h / 2 {
		row := f.UV[y*f.UVStride : y*f.UVStride+w]
		cr := 64 + (y*2)*128/h + shift%32
		for x := 0; x < w; x += 2 {
			row[x] = byte(64 + x*128/w)
			row[x+1] = byte(cr)
		}
	}
	c := cellSize(w)
	fill(f, c, 0, (cells+2)*c, 3*c, black)
	bits := codeOf(n)
	for i, on := range bits {
		v := byte(black)
		if on {
			v = white
		}
		fill(f, (2+i)*c, c, c, c, v)
	}
	p := byte(black)
	if flash {
		p = white
	}
	fill(f, w-6*c, h-6*c, 4*c, 4*c, p)
}

// codeOf is the barcode's cells for n.
func codeOf(n uint32) [cells]bool {
	var b [cells]bool
	b[0], b[2] = true, true
	for i := range codeBits {
		b[4+i] = n>>(codeBits-1-i)&1 == 1
	}
	chk := check(n)
	for i := range 4 {
		b[4+codeBits+i] = chk>>(3-i)&1 == 1
	}
	return b
}

func check(n uint32) uint32 {
	var c uint32
	for i := 0; i < codeBits; i += 4 {
		c ^= n >> i & 0xf
	}
	return c ^ 0x5
}

// fill paints a w×h rectangle at x, y: brightness v, colour neutral.
func fill(f *Frame, x, y, w, h int, v byte) {
	x, y = max(x, 0)&^1, max(y, 0)&^1
	w, h = min(w, f.W-x), min(h, f.H-y)
	if w <= 0 || h <= 0 {
		return // off the frame (one too small for the code)
	}
	for r := y; r < y+h; r++ {
		row := f.Y[r*f.YStride+x : r*f.YStride+x+w]
		for i := range row {
			row[i] = v
		}
	}
	for r := y / 2; r < (y+h)/2; r++ {
		row := f.UV[r*f.UVStride+x : r*f.UVStride+x+w]
		for i := range row {
			row[i] = neutral
		}
	}
}

// mean is the average brightness of the middle half of a rectangle.
func mean(f Frame, x, y, w, h int) float64 {
	return meanF(f, float64(x), float64(y), float64(w), float64(h))
}

// meanF is mean on a rectangle with fractional corners.
func meanF(f Frame, x, y, w, h float64) float64 {
	x0, y0, x1, y1 := int(x+w/4), int(y+h/4), int(x+w*3/4), int(y+h*3/4)
	var sum, n int
	for r := y0; r < y1 && r < f.H; r++ {
		for c := x0; c < x1 && c < f.W; c++ {
			sum += int(f.Y[r*f.YStride+c])
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

// ReadCode reads the frame number from a pattern frame; false when the
// barcode isn't there or doesn't check. A frame scaled down to a half or
// a quarter of the size it was drawn at (a simulcast layer) reads too:
// its cells are the drawn size's, scaled, which rounding can make
// differ from cellSize at the smaller width.
func ReadCode(f Frame) (uint32, bool) {
	for _, c := range cellSizes(f.W) {
		if n, ok := readCode(f, c); ok {
			return n, true
		}
	}
	return 0, false
}

// cellSizes are the cell sizes a frame w wide may carry: drawn at w, or
// at twice or four times w and scaled down.
func cellSizes(w int) []float64 {
	out := []float64{float64(cellSize(w))}
	for _, k := range []int{2, 4} {
		if c := float64(cellSize(w*k)) / float64(k); c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	return out
}

func readCode(f Frame, c float64) (uint32, bool) {
	var b [cells]bool
	for i := range b {
		b[i] = meanF(f, float64(2+i)*c, c, c, c) > (black+white)/2
	}
	if !b[0] || b[1] || !b[2] || b[3] {
		return 0, false
	}
	var n uint32
	for i := range codeBits {
		if b[4+i] {
			n |= 1 << (codeBits - 1 - i)
		}
	}
	var chk uint32
	for i := range 4 {
		if b[4+codeBits+i] {
			chk |= 1 << (3 - i)
		}
	}
	return n, chk == check(n)
}

// Flash reports whether a pattern frame's flash patch is lit (at the
// cell size its barcode reads at).
func Flash(f Frame) bool {
	c := float64(cellSize(f.W))
	for _, cc := range cellSizes(f.W) {
		if _, ok := readCode(f, cc); ok {
			c = cc
			break
		}
	}
	w, h := float64(f.W), float64(f.H)
	return meanF(f, w-6*c, h-6*c, 4*c, 4*c) > (black+white)/2
}

// PSNR compares two frames of one size, brightness and colour separately,
// in dB (+Inf when they're the same).
func PSNR(a, b Frame) (y, uv float64) {
	return psnr(a.Y, b.Y, a.W, a.H, a.YStride, b.YStride), psnr(a.UV, b.UV, a.W, a.H/2, a.UVStride, b.UVStride)
}

func psnr(a, b []byte, w, h, sa, sb int) float64 {
	var se float64
	for y := range h {
		ra, rb := a[y*sa:y*sa+w], b[y*sb:y*sb+w]
		for x := range ra {
			d := float64(int(ra[x]) - int(rb[x]))
			se += d * d
		}
	}
	if se == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/(se/float64(w*h)))
}
