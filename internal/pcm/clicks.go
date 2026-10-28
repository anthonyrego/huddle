package pcm

import "math"

// Clicks for measuring latency: a train of short tone bursts, and finding
// where bursts begin in what came out the other end.

// ClickTrain is n bursts every `every` samples, the first at `first`: each
// 5 ms of 1 kHz at half scale, faded in and out so a codec keeps its shape.
func ClickTrain(n, first, every, rate int) []float32 {
	out := make([]float32, first+n*every)
	burst := rate / 200
	for c := range n {
		at := first + c*every
		for i := range burst {
			env := math.Sin(math.Pi * float64(i) / float64(burst))
			out[at+i] = float32(0.5 * env * math.Sin(2*math.Pi*1000*float64(i)/float64(rate)))
		}
	}
	return out
}

// Onsets finds where bursts begin: the first sample of a 1 ms block whose
// mean absolute value passes threshold after at least quiet samples below
// it. The block's own onset is refined to the first sample over threshold.
func Onsets(s []float32, threshold float32, quiet, rate int) []int {
	block := rate / 1000
	var at []int
	lastLoud := -quiet - 1
	for b := 0; (b+1)*block <= len(s); b++ {
		var sum float32
		for _, v := range s[b*block : (b+1)*block] {
			sum += float32(math.Abs(float64(v)))
		}
		if sum/float32(block) < threshold {
			continue
		}
		start := b * block
		if start-lastLoud > quiet {
			for i := max(start-block, 0); i < start+block; i++ {
				if float32(math.Abs(float64(s[i]))) >= threshold {
					start = i
					break
				}
			}
			at = append(at, start)
		}
		lastLoud = (b + 1) * block
	}
	return at
}
