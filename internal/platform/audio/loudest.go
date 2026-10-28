package audio

// loudest follows which of a device's input channels carries the voice.
// An audio interface has several inputs and the mic is on one of them; the
// others are silent or carry something else (an instrument, a second mic
// nobody is at). Each channel's power is smoothed over about a second (a
// tap's 100 ms at a time), and the pick moves only to a channel 6 dB above
// the current one, so it doesn't flap between two inputs carrying the same
// voice. Used without voice processing; with it, channel 0 is the cleaned
// mic.
type loudest struct {
	power []float64
	cur   int
}

// loudestSmooth is how much of a channel's power each buffer replaces.
const loudestSmooth = 0.1

// update takes one buffer's channels (each the same length) and returns
// the channel to use.
func (p *loudest) update(chans [][]float32) int {
	if len(chans) != len(p.power) {
		p.power, p.cur = make([]float64, len(chans)), 0
	}
	for c, ch := range chans {
		var sum float64
		for _, v := range ch {
			sum += float64(v) * float64(v)
		}
		if len(ch) > 0 {
			sum /= float64(len(ch))
		}
		p.power[c] += loudestSmooth * (sum - p.power[c])
	}
	best := p.cur
	for c, pw := range p.power {
		if pw > p.power[best] {
			best = c
		}
	}
	if best != p.cur && p.power[best] > 4*p.power[p.cur] {
		p.cur = best
	}
	return p.cur
}
