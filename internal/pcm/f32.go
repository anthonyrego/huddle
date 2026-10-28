package pcm

import (
	"encoding/binary"
	"math"
)

// Float32 PCM (full scale ±1), the format voice runs in: conversions to
// and from s16 bytes, levels, and the comparisons the voice gates score
// with.

// F32FromS16 converts mono s16 bytes to float32 samples.
func F32FromS16(b []byte) []float32 {
	out := make([]float32, len(b)/BytesPerSample)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768
	}
	return out
}

// S16FromF32 converts float32 samples to mono s16 bytes, clipping at full
// scale.
func S16FromF32(s []float32) []byte {
	out := make([]byte, len(s)*BytesPerSample)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(ToS16(v)))
	}
	return out
}

// ToS16 converts one sample, clipping at full scale.
func ToS16(v float32) int16 {
	x := math.Round(float64(v) * 32768)
	return int16(max(min(x, 32767), -32768))
}

// LevelF32 is the RMS level of s in dBFS, Silence for all zeros.
func LevelF32(s []float32) float64 {
	if len(s) == 0 {
		return Silence
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(s)))
	if rms == 0 {
		return Silence
	}
	return max(20*math.Log10(rms), Silence)
}

// Correlation is the Pearson correlation of a and b[lag:], over the
// samples both have.
func Correlation(a, b []float32, lag int) float64 {
	if lag < 0 {
		a, b, lag = b, a, -lag
	}
	if lag >= len(b) {
		return 0
	}
	b = b[lag:]
	n := min(len(a), len(b))
	if n == 0 {
		return 0
	}
	var sa, sb float64
	for i := 0; i < n; i++ {
		sa += float64(a[i])
		sb += float64(b[i])
	}
	ma, mb := sa/float64(n), sb/float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := float64(a[i])-ma, float64(b[i])-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// BestLag finds where a appears in b: the lag in [0, maxLag] at which b
// lags a with the highest correlation, found on a coarse envelope first and
// then sample by sample around it, and that correlation.
func BestLag(a, b []float32, maxLag int) (lag int, corr float64) {
	const step = 48 // 1 ms at 48 kHz
	ea, eb := envelope(a, step), envelope(b, step)
	best, bestC := 0, math.Inf(-1)
	for l := 0; l <= maxLag/step && l < len(eb); l++ {
		if c := Correlation(ea, eb, l); c > bestC {
			best, bestC = l, c
		}
	}
	lo, hi := max(best*step-2*step, 0), min(best*step+2*step, maxLag)
	lag, corr = lo, math.Inf(-1)
	for l := lo; l <= hi; l++ {
		if c := Correlation(a, b, l); c > corr {
			lag, corr = l, c
		}
	}
	return lag, corr
}

// envelope is the mean absolute value of each block of n samples.
func envelope(s []float32, n int) []float32 {
	out := make([]float32, len(s)/n)
	for i := range out {
		var sum float32
		for _, v := range s[i*n : (i+1)*n] {
			sum += float32(math.Abs(float64(v)))
		}
		out[i] = sum / float32(n)
	}
	return out
}

// Segmental scores b against a window by window, where the delay between
// them may change (a jitter buffer adds and removes delay as it goes): each
// window of a with speech in it (louder than -45 dBFS) is found in b at its
// own lag, up to maxLag later than where a has it. It returns the mean of
// the windows' correlations, the worst one, and each window's lag.
func Segmental(a, b []float32, win, maxLag int) (mean, worst float64, lags []int) {
	worst = 1
	var sum float64
	n := 0
	for i := 0; i+win <= len(a); i += win {
		seg := a[i : i+win]
		if LevelF32(seg) < -45 {
			continue
		}
		end := min(i+win+maxLag, len(b))
		if i >= end {
			break
		}
		lag, c := BestLag(seg, b[i:end], maxLag)
		sum += c
		n++
		worst = min(worst, c)
		lags = append(lags, lag)
	}
	if n == 0 {
		return 0, 0, nil
	}
	return sum / float64(n), worst, lags
}
