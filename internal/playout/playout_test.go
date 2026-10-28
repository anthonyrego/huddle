package playout

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/jitter"
	"github.com/anthonyrego/huddle/internal/netsim"
	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
)

// tone is a source of a constant sine (or silence), whose level and target
// the test sets.
type tone struct {
	amp, phase    float64
	level, target time.Duration
	pulls         int
}

func (s *tone) Pull(out []float32) {
	s.pulls++
	for i := range out {
		s.phase += 2 * math.Pi * 440 / 48000
		out[i] = float32(s.amp * math.Sin(s.phase))
	}
}
func (s *tone) Level() time.Duration  { return s.level }
func (s *tone) Target() time.Duration { return s.target }

func TestMixAndLevels(t *testing.T) {
	m := New()
	a := &tone{amp: 0.1}
	b := &tone{amp: 0.01}
	m.Add("ta", "alice", a)
	m.Add("tb", "bob", b)
	out := make([]float32, FrameSamples)
	m.Mix(out)
	// Same phase, so the mix is one sine of 0.11.
	if l := pcm.LevelF32(out); math.Abs(l-(20*math.Log10(0.11/math.Sqrt2))) > 0.2 {
		t.Fatalf("mix level %.2f dBFS", l)
	}
	ls := m.Levels()
	if len(ls) != 2 || ls[0].Identity != "alice" || math.Abs(float64(ls[0].DBFS)+23.0) > 0.2 || math.Abs(float64(ls[1].DBFS)+43.0) > 0.2 {
		t.Fatalf("levels %+v", ls)
	}
	// Bob stops: his level falls at 50 dB/s, not at once.
	b.amp = 0
	m.Mix(out)
	if l := m.Levels()[1].DBFS; math.Abs(float64(l)+43.5) > 0.2 {
		t.Fatalf("bob's level a frame after he stopped: %.2f", l)
	}
	m.Remove("tb")
	if ls := m.Levels(); len(ls) != 1 {
		t.Fatalf("levels after bob left: %+v", ls)
	}
}

func TestLimit(t *testing.T) {
	m := New()
	for _, id := range []string{"a", "b", "c"} {
		m.Add(id, id, &tone{amp: 0.9})
	}
	out := make([]float32, FrameSamples)
	m.Mix(out)
	for _, v := range out {
		if v > 1 || v < -1 {
			t.Fatalf("mix reached %v", v)
		}
	}
}

func TestStretchOnlyAtSilence(t *testing.T) {
	out := make([]float32, FrameSamples)
	for _, c := range []struct {
		name          string
		amp           float64
		level, target time.Duration
		dropped, silent int // in 40 frames: at most one in five, once the smoothed offset is past 25 ms
	}{
		{"on target", 0, 40 * time.Millisecond, 40 * time.Millisecond, 0, 0},
		{"over, quiet", 0, 100 * time.Millisecond, 40 * time.Millisecond, 6, 0},
		{"under, quiet", 0, 40 * time.Millisecond, 100 * time.Millisecond, 0, 6},
		{"over, talking: leaves it", 0.1, 100 * time.Millisecond, 40 * time.Millisecond, 0, 0},
		{"under, talking: leaves it", 0.1, 40 * time.Millisecond, 100 * time.Millisecond, 0, 0},
	} {
		m := New()
		s := &tone{amp: c.amp, level: c.level, target: c.target}
		m.Add("t", "p", s)
		for range 40 {
			m.Mix(out)
		}
		st := m.Stats()["t"]
		if st.Dropped != c.dropped || st.Silent != c.silent || s.pulls != 40+st.Dropped-st.Silent {
			t.Errorf("%s: %+v and %d pulls in 40 frames, want %d skipped, %d silent", c.name, st, s.pulls, c.dropped, c.silent)
		}
	}
}

// TestFollowsTarget runs speech with pauses through a network that turns
// jittery, a jitter buffer and the mixer: the mixer's stretching at
// silence brings the buffer up to its new target, so few packets come late.
func TestFollowsTarget(t *testing.T) {
	src := speechLike(30 * 48000)
	enc, _ := opus.NewEncoder(opus.Options{})
	defer enc.Close()
	dec, _ := opus.NewDecoder()
	buf := jitter.New(dec, jitter.Config{})
	m := New()
	m.Add("t", "p", buf)

	t0 := time.Unix(1000, 0)
	calm := netsim.New(0, 0, 0, 1)
	rough := netsim.New(0, 40*time.Millisecond, 0, 2)
	type arrival struct {
		p  jitter.Packet
		at time.Time
	}
	var arrivals []arrival
	pkt := make([]byte, opus.MaxPacket)
	for k := 0; (k+1)*opus.FrameSamples <= len(src); k++ {
		n, _ := enc.Encode(src[k*opus.FrameSamples:(k+1)*opus.FrameSamples], pkt)
		sentAt := t0.Add(time.Duration(k) * 20 * time.Millisecond)
		sim := calm
		if k >= 250 { // jittery after 5 s
			sim = rough
		}
		at, _ := sim.Fate(sentAt.Add(5 * time.Millisecond))
		arrivals = append(arrivals, arrival{jitter.Packet{Seq: uint16(k), TS: uint32(k * opus.FrameSamples), Payload: append([]byte(nil), pkt[:n]...)}, at})
	}
	out := make([]float32, FrameSamples)
	i := 0
	var lateAt10s int
	for f := 0; f < 3000; f++ {
		now := t0.Add(time.Duration(f) * 10 * time.Millisecond)
		for ; i < len(arrivals) && !arrivals[i].at.After(now); i++ {
			buf.Push(arrivals[i].p, arrivals[i].at)
		}
		m.Mix(out)
		if f == 1000 {
			lateAt10s = buf.Stats().Late
		}
	}
	st := buf.Stats()
	ts := m.Stats()["t"]
	t.Logf("target %v, level %v, late %d (by 10 s: %d), lost %d, stretches: %d skipped, %d silent", st.Target, st.Level, st.Late, lateAt10s, st.Lost, ts.Dropped, ts.Silent)
	if ts.Silent == 0 {
		t.Fatal("never stretched up to the higher target")
	}
	if late := st.Late - lateAt10s; late > 10 {
		t.Fatalf("%d packets late in the 20 s after the target moved", late)
	}
}

// speechLike is a voice-like signal: harmonics with syllables and pauses.
func speechLike(n int) []float32 {
	out := make([]float32, n)
	phase := 0.0
	for i := range out {
		tm := float64(i) / 48000
		phase += 2 * math.Pi * (140 + 30*math.Sin(2*math.Pi*0.7*tm)) / 48000
		var v float64
		for h := 1; h <= 8; h++ {
			v += math.Sin(float64(h)*phase) / float64(h)
		}
		syll := math.Max(0, math.Sin(2*math.Pi*4*tm))
		if math.Mod(tm, 3) > 2 { // a one-second pause every three
			syll = 0
		}
		out[i] = float32(0.2 * v * syll)
	}
	return out
}

type slowSpeaker struct{ n int }

func (s *slowSpeaker) Write(ctx context.Context, frame []float32) error {
	s.n++
	if s.n == 5 {
		return errors.New("device gone")
	}
	return nil
}

func TestPlayStopsOnSpeakerError(t *testing.T) {
	m := New()
	m.Add("t", "p", &tone{amp: 0.1})
	spk := &slowSpeaker{}
	if err := Play(context.Background(), m, spk); err == nil || spk.n != 5 {
		t.Fatalf("Play = %v after %d writes", err, spk.n)
	}
}
