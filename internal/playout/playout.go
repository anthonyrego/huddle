// Package playout mixes everyone you hear into one stream for the
// speakers, 10 ms at a time, on the output device's clock: Play writes a
// frame to the speaker and the speaker's Write blocks until the device
// wants more, so the device sets the pace and every source is pulled
// exactly as fast as it plays.
//
// Each source (one remote track's jitter buffer) is stretched lightly at
// silence to follow its target delay: when it holds more than its target,
// a quiet 10 ms frame is skipped; when it holds less, 10 ms of silence is
// played instead of pulling, at most one frame in five and never in the
// middle of speech. The mixer also keeps each participant's level for the
// talking indicators.
package playout

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/pcm"
)

// FrameSamples is one 10 ms frame at 48 kHz.
const FrameSamples = 480

// Source is one remote track: a jitter buffer.
type Source interface {
	Pull(out []float32)
	Level() time.Duration  // audio held ahead of what plays next
	Target() time.Duration // the delay it aims for
}

// Speaker takes the mix: Write queues one 10 ms frame and blocks while the
// device has enough queued (rtc.Speaker).
type Speaker interface {
	Write(ctx context.Context, frame []float32) error
}

// Tuning.
const (
	// QuietDBFS is how quiet a frame is to count as silence, for stretching.
	QuietDBFS = -50
	// TalkingDBFS is the level at which a participant counts as talking.
	TalkingDBFS = -45
	// skew is how far a source may be from its target before it is
	// stretched.
	skew = 25 * time.Millisecond
	// stretchEvery is the fewest frames between two stretches of a source
	// (so one 10 ms frame in five at most).
	stretchEvery = 4
	// release is how fast a shown level falls, in dB per frame (50 dB/s).
	release = 0.5
)

// Level is how loud a participant is now, for talking indicators: the
// frame's RMS, held and falling at 50 dB/s.
type Level struct {
	Identity string
	DBFS     float32
}

// TrackStats count a track's stretches.
type TrackStats struct {
	Identity        string
	Dropped, Silent int // quiet frames skipped, silent frames played instead of pulling
}

// Mixer mixes sources. Its methods may be called from any goroutine.
type Mixer struct {
	mu     sync.Mutex
	tracks map[string]*track // by track id
	levels map[string]float64
	sum    []float64
	who    map[string][]float32 // a participant's frame, summed over their tracks
}

type track struct {
	id, who     string
	src         Source
	frame       []float32
	quiet       bool
	sinceAdjust int
	// off is how far the source is from its target, smoothed over about
	// 200 ms: the level jumps with every packet that arrives, and acting
	// on each jump would stretch back and forth.
	off time.Duration
	st  TrackStats
}

// New returns an empty mixer.
func New() *Mixer {
	return &Mixer{tracks: map[string]*track{}, levels: map[string]float64{}, sum: make([]float64, FrameSamples), who: map[string][]float32{}}
}

// Add mixes in a track of participant who.
func (m *Mixer) Add(id, who string, src Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracks[id] = &track{id: id, who: who, src: src, frame: make([]float32, FrameSamples), sinceAdjust: stretchEvery, st: TrackStats{Identity: who}}
}

// Remove stops mixing a track.
func (m *Mixer) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tracks[id]
	if t == nil {
		return
	}
	delete(m.tracks, id)
	still := false
	for _, o := range m.tracks {
		still = still || o.who == t.who
	}
	if !still {
		delete(m.levels, t.who)
		delete(m.who, t.who)
	}
}

// Mix fills out (FrameSamples) with the next 10 ms of everyone.
func (m *Mixer) Mix(out []float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.sum)
	for _, f := range m.who {
		clear(f)
	}
	for _, t := range m.tracks {
		t.pull()
		for i, v := range t.frame {
			m.sum[i] += float64(v)
		}
		w := m.who[t.who]
		if w == nil {
			w = make([]float32, FrameSamples)
			m.who[t.who] = w
		}
		for i, v := range t.frame {
			w[i] += v
		}
	}
	for who, f := range m.who {
		l := pcm.LevelF32(f)
		if old, ok := m.levels[who]; ok && l < old-release {
			l = old - release
		}
		m.levels[who] = l
	}
	for i, v := range m.sum {
		out[i] = limit(v)
	}
}

// pull takes the track's next frame, stretching at silence.
func (t *track) pull() {
	t.off += (t.src.Level() - t.src.Target() - t.off) / 20
	ready := t.quiet && t.sinceAdjust >= stretchEvery
	t.sinceAdjust++
	switch {
	case ready && t.off > skew:
		t.src.Pull(t.frame)
		if pcm.LevelF32(t.frame) < QuietDBFS {
			t.src.Pull(t.frame) // skip the quiet one
			t.st.Dropped++
			t.sinceAdjust = 0
			t.off -= 10 * time.Millisecond
		}
	case ready && t.off < -skew:
		clear(t.frame) // silence, leaving the source where it is
		t.st.Silent++
		t.sinceAdjust = 0
		t.off += 10 * time.Millisecond
	default:
		t.src.Pull(t.frame)
	}
	t.quiet = pcm.LevelF32(t.frame) < QuietDBFS
}

// limit soft-clips the mix above 0.8 so that people talking over each other
// never wrap around.
func limit(v float64) float32 {
	const knee = 0.8
	a := math.Abs(v)
	if a <= knee {
		return float32(v)
	}
	a = knee + (1-knee)*math.Tanh((a-knee)/(1-knee))
	return float32(math.Copysign(a, v))
}

// Levels returns each participant's level, loudest first.
func (m *Mixer) Levels() []Level {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := make([]Level, 0, len(m.levels))
	for who, l := range m.levels {
		ls = append(ls, Level{Identity: who, DBFS: float32(l)})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].DBFS > ls[j].DBFS })
	return ls
}

// Stats returns each track's stretches, by track id.
func (m *Mixer) Stats() map[string]TrackStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := map[string]TrackStats{}
	for id, t := range m.tracks {
		st[id] = t.st
	}
	return st
}

// Play mixes into spk until ctx ends or the speaker fails. The speaker's
// Write paces it.
func Play(ctx context.Context, m *Mixer, spk Speaker) error {
	frame := make([]float32, FrameSamples)
	for ctx.Err() == nil {
		m.Mix(frame)
		if err := spk.Write(ctx, frame); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}
