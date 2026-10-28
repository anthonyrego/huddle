package jitter

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/netsim"
	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
)

// fakeDec stamps every frame so the tests can see what the buffer did: a
// decoded packet k plays k+1, a frame recovered by FEC 10000+k+1, a
// concealed one -1, silence 0.
type fakeDec struct{}

const toc20ms = 1 << 3 // SILK NB, 20 ms, one frame

func payload(k int) []byte { return []byte{toc20ms, byte(k >> 8), byte(k)} }
func dtxPayload() []byte   { return []byte{toc20ms} }

func idx(p []byte) int { return int(p[1])<<8 | int(p[2]) }

func (fakeDec) Decode(p []byte, out []float32) (int, error) {
	if len(p) < 3 {
		return 0, errors.New("short")
	}
	fill(out[:opus.FrameSamples], float32(idx(p)+1))
	return opus.FrameSamples, nil
}
func (fakeDec) DecodeFEC(next []byte, out []float32) (int, error) {
	fill(out, float32(10000+idx(next)-1+1))
	return len(out), nil
}
func (fakeDec) Conceal(out []float32) (int, error) { fill(out, -1); return len(out), nil }
func (fakeDec) HasFEC() bool                       { return true }
func (fakeDec) Close() error                       { return nil }

func fill(s []float32, v float32) {
	for i := range s {
		s[i] = v
	}
}

// sent is one packet on the wire: when it left and when (if) it arrives.
type sent struct {
	pkt    Packet
	sentAt time.Time
	at     time.Time
	drop   bool
}

var t0 = time.Unix(1000, 0)

// stream makes n 20 ms packets from frame 0, sequence numbers from seq0,
// sent in real time.
func stream(n int, seq0 uint16, ts0 uint32) []sent {
	var s []sent
	for k := range n {
		at := t0.Add(time.Duration(k) * 20 * time.Millisecond)
		s = append(s, sent{pkt: Packet{Seq: seq0 + uint16(k), TS: ts0 + uint32(k*opus.FrameSamples), Payload: payload(k)}, sentAt: at, at: at.Add(5 * time.Millisecond)})
	}
	return s
}

// through applies a network simulation to a stream's arrival times.
func through(s []sent, sim *netsim.Sim) []sent {
	for i := range s {
		s[i].at, s[i].drop = sim.Fate(s[i].sentAt.Add(5 * time.Millisecond))
	}
	return s
}

// run plays the stream into a buffer on a 10 ms output clock for d,
// returning each 10 ms frame's first sample (its stamp) and when it played.
type frame struct {
	stamp float32
	at    time.Time
}

func run(b *Buffer, s []sent, d time.Duration) []frame {
	var pending []sent
	for _, p := range s {
		if !p.drop {
			pending = append(pending, p)
		}
	}
	var frames []frame
	out := make([]float32, 480)
	for t := t0; t.Before(t0.Add(d)); t = t.Add(10 * time.Millisecond) {
		if len(pending) == 0 && b.Level() == 0 {
			break // all played: what follows would be the end of the stream
		}
		// Everything that has arrived by now, in arrival order.
		for {
			best := -1
			for i, p := range pending {
				if !p.drop && !p.at.After(t) && (best < 0 || p.at.Before(pending[best].at)) {
					best = i
				}
			}
			if best < 0 {
				break
			}
			b.Push(pending[best].pkt, pending[best].at)
			pending = append(pending[:best], pending[best+1:]...)
		}
		b.Pull(out)
		frames = append(frames, frame{out[0], t})
	}
	return frames
}

// played lists, in order, what each 20 ms of stream became (one stamp per
// two 10 ms frames once playing started), with runs of silence collapsed.
func played(frames []frame) []float32 {
	var seq []float32
	started := false
	for i := 0; i < len(frames); i++ {
		f := frames[i].stamp
		if !started && f == 0 {
			continue
		}
		started = true
		if len(seq) > 0 && seq[len(seq)-1] == f && f == 0 {
			continue
		}
		seq = append(seq, f)
		if f != 0 && i+1 < len(frames) && frames[i+1].stamp == f {
			i++ // the second half of the same 20 ms
		}
	}
	return seq
}

// latency is how long after it was sent each decoded frame k began to play.
func latency(frames []frame, s []sent) map[int]time.Duration {
	lat := map[int]time.Duration{}
	for _, f := range frames {
		if f.stamp >= 1 && f.stamp < 10000 {
			k := int(f.stamp) - 1
			if _, ok := lat[k]; !ok && k < len(s) {
				lat[k] = f.at.Sub(s[k].sentAt)
			}
		}
	}
	return lat
}

func want(t *testing.T, got []float32, w ...float32) {
	t.Helper()
	if fmt.Sprint(got[:min(len(got), len(w))]) != fmt.Sprint(w) {
		t.Fatalf("played %v\nwant   %v", got[:min(len(got), len(w)+4)], w)
	}
}

func TestSteady(t *testing.T) {
	s := stream(100, 7, 1234)
	b := New(fakeDec{}, Config{})
	frames := run(b, s, 2500*time.Millisecond)
	got := played(frames)
	for k := range 100 {
		if got[k] != float32(k+1) {
			t.Fatalf("frame %d played %v (%v)", k, got[k], got[:k+1])
		}
	}
	st := b.Stats()
	if st.Lost != 0 || st.Late != 0 || st.Underrun != 0 || st.Target != 40*time.Millisecond {
		t.Fatalf("stats %+v", st)
	}
	for k, l := range latency(frames, s) {
		if l < 20*time.Millisecond || l > 60*time.Millisecond {
			t.Fatalf("frame %d played %v after it was sent, want 20–60 ms", k, l)
		}
	}
}

func TestLossUsesFEC(t *testing.T) {
	s := stream(40, 0, 0)
	for _, k := range []int{10, 20, 21, 30} {
		s[k].drop = true
	}
	b := New(fakeDec{}, Config{})
	got := played(run(b, s, time.Second))
	// 10 and 30 come back from the next packet's FEC; 20 is concealed (21
	// is missing too), 21 comes from 22's FEC.
	// (Two in a row are first seen as an underrun, concealed while waiting.)
	has := map[float32]bool{}
	for _, f := range got {
		has[f] = true
	}
	for _, f := range []float32{10011, 10022, 10031, 1, 10, 12, 20, 23, 30, 32, 40, -1} {
		if !has[f] {
			t.Errorf("%v never played: %v", f, got)
		}
	}
	for _, f := range []float32{11, 21, 22, 31, 10021} {
		if has[f] {
			t.Errorf("%v played: %v", f, got)
		}
	}
	if st := b.Stats(); st.Lost != 4 || st.FEC != 3 || st.PLC != 1 || st.Concealed() != 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestReorder(t *testing.T) {
	s := stream(100, 65500, math.MaxUint32-50*960) // wraps both
	for k := 5; k < 100; k += 10 {
		s[k].at = s[k].at.Add(21 * time.Millisecond) // overtaken by k+1, still in time
	}
	b := New(fakeDec{}, Config{})
	got := played(run(b, s, 2500*time.Millisecond))
	for k := range 100 {
		if got[k] != float32(k+1) {
			t.Fatalf("frame %d played %v", k, got[k])
		}
	}
	if st := b.Stats(); st.Lost != 0 || st.Late != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestLateIsDropped(t *testing.T) {
	s := stream(50, 0, 0)
	s[20].at = s[20].at.Add(500 * time.Millisecond)
	b := New(fakeDec{}, Config{})
	got := played(run(b, s, 1500*time.Millisecond))
	if got[20] != 10000+21 {
		t.Fatalf("frame 20 played %v, want it recovered from 21's FEC", got[20])
	}
	if st := b.Stats(); st.Late != 1 || st.Lost != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDuplicates(t *testing.T) {
	s := stream(30, 0, 0)
	s = append(s, s[10], s[11])
	b := New(fakeDec{}, Config{})
	run(b, s, time.Second)
	if st := b.Stats(); st.Duplicate+st.Late != 2 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestDTX: a talk spurt, a DTX packet, 2 s with nothing sent, the next
// spurt. The silence isn't an underrun, and the second spurt starts at the
// target delay, not 2 s late.
func TestDTX(t *testing.T) {
	spurt := stream(51, 100, 0)
	spurt[50].pkt.Payload = dtxPayload()
	for k := 150; k < 200; k++ { // the sequence goes on; the time jumped
		at := t0.Add(time.Duration(k) * 20 * time.Millisecond)
		spurt = append(spurt, sent{pkt: Packet{Seq: 100 + 51 + uint16(k-150), TS: uint32(k * 960), Payload: payload(k)}, sentAt: at, at: at.Add(5 * time.Millisecond)})
	}
	b := New(fakeDec{}, Config{})
	frames := run(b, spurt, 5*time.Second)
	st := b.Stats()
	if st.Underrun != 0 || st.Lost != 0 || st.Skipped == 0 {
		t.Fatalf("stats %+v", st)
	}
	for k, l := range latency(frames, spurt) {
		if k >= 150 && (l < 20*time.Millisecond || l > 60*time.Millisecond) {
			t.Fatalf("frame %d after the silence played %v after it was sent", k, l)
		}
	}
}

func TestJitterRaisesThenLowersTarget(t *testing.T) {
	s := through(stream(500, 0, 0), netsim.New(0, 40*time.Millisecond, 0, 3))
	calm := stream(1000, 500, 500*960)
	for i := range calm {
		calm[i].pkt.Payload = payload(500 + i)
		calm[i].sentAt = calm[i].sentAt.Add(10 * time.Second)
		calm[i].at = calm[i].at.Add(10 * time.Second)
	}
	b := New(fakeDec{}, Config{})
	run(b, s, 10*time.Second)
	st := b.Stats()
	t.Logf("after 10 s of 40 ms jitter: target %v, jitter %v, late %d, lost %d, underrun %v", st.Target, st.Jitter, st.Late, st.Lost, dur(int64(st.Underrun)))
	if st.Target < 80*time.Millisecond || st.Target > 120*time.Millisecond {
		t.Fatalf("target %v under 40 ms jitter, want 80–120 ms", st.Target)
	}
	if st.Late > 5 {
		t.Fatalf("%d late of 500", st.Late)
	}
	b2 := New(fakeDec{}, Config{})
	run(b2, append(s, calm...), 30*time.Second)
	if st := b2.Stats(); st.Target != 40*time.Millisecond {
		t.Fatalf("target %v 20 s after the jitter stopped, want 40 ms", st.Target)
	}
}

// TestPaddingThenSpeech is the whiteboard's track as a new subscriber gets
// it: LiveKit's 5 s of padding, whose timestamps step 961 every ~21.8 ms
// (slower than they arrive), then quiet, then an answer at 20 ms a frame.
// The answer is measured on its own, at the floor, not at the most the
// padding's drift left in the window. A mic's DTX frame every 400 ms keeps
// the window, so its jitter carries across a pause.
func TestPaddingThenSpeech(t *testing.T) {
	var s []sent
	ts := uint32(0)
	for k := range 251 {
		at := t0.Add(time.Duration(k) * 21800 * time.Microsecond)
		s = append(s, sent{pkt: Packet{Seq: uint16(k), TS: ts, Payload: payload(k)}, sentAt: at, at: at})
		ts += 961
	}
	answer := stream(120, 251, ts+407438)
	for i := range answer {
		answer[i].pkt.Payload = payload(251 + i)
		answer[i].sentAt = answer[i].sentAt.Add(13 * time.Second)
		answer[i].at = answer[i].at.Add(13 * time.Second)
	}
	pad := New(fakeDec{}, Config{})
	run(pad, s, 6*time.Second)
	t.Logf("after the padding alone: target %v", pad.Stats().Target)
	b := New(fakeDec{}, Config{})
	run(b, append(s, answer...), 16*time.Second)
	if st := b.Stats(); st.Target != 40*time.Millisecond {
		t.Fatalf("target %v during the answer, want 40 ms", st.Target)
	}

	// 40 ms of jitter, a 3 s pause holding a DTX frame every 400 ms, then
	// calm speech: the pause doesn't wipe what the jitter taught.
	jittery := through(stream(200, 0, 0), netsim.New(0, 40*time.Millisecond, 0, 3))
	var pause []sent
	for k := range 150 {
		if k%20 == 0 {
			at := t0.Add(4*time.Second + time.Duration(k)*20*time.Millisecond)
			pause = append(pause, sent{pkt: Packet{Seq: uint16(200 + k/20), TS: uint32((200 + k) * opus.FrameSamples), Payload: dtxPayload()}, sentAt: at, at: at.Add(5 * time.Millisecond)})
		}
	}
	calm := stream(20, uint16(200+len(pause)), 350*opus.FrameSamples)
	for i := range calm {
		calm[i].pkt.Payload = payload(400 + i)
		calm[i].sentAt = calm[i].sentAt.Add(7 * time.Second)
		calm[i].at = calm[i].at.Add(7 * time.Second)
	}
	b2 := New(fakeDec{}, Config{})
	run(b2, append(append(jittery, pause...), calm...), 8*time.Second)
	if st := b2.Stats(); st.Target <= 40*time.Millisecond {
		t.Fatalf("target %v after a pause with DTX: the jitter was forgotten", st.Target)
	}
}

// TestStallThenBurst: 600 ms with nothing, then it all arrives at once.
// The buffer conceals through the stall, and trims the burst back.
func TestStallThenBurst(t *testing.T) {
	s := stream(150, 0, 0)
	for k := 40; k < 70; k++ {
		s[k].at = s[70].at
	}
	b := New(fakeDec{}, Config{})
	run(b, s, 4*time.Second)
	st := b.Stats()
	t.Logf("%+v", st)
	if st.Underrun == 0 {
		t.Fatal("no underrun during the stall")
	}
	if st.Level > 100*time.Millisecond {
		t.Fatalf("level %v at the end, want back near the target", st.Level)
	}
}

// TestSpeechThroughLossAndJitter decodes real Opus speech through 5% and
// 10% loss with 40 ms jitter and scores it against the clean decode.
func TestSpeechThroughLossAndJitter(t *testing.T) {
	src := speechFixture(t)
	enc, err := opus.NewEncoder(opus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var s []sent
	buf := make([]byte, opus.MaxPacket)
	seq := uint16(0)
	for k := 0; (k+1)*opus.FrameSamples <= len(src); k++ {
		n, err := enc.Encode(src[k*opus.FrameSamples:(k+1)*opus.FrameSamples], buf)
		if err != nil {
			t.Fatal(err)
		}
		at := t0.Add(time.Duration(k) * 20 * time.Millisecond)
		s = append(s, sent{pkt: Packet{Seq: seq, TS: uint32(k * opus.FrameSamples), Payload: append([]byte(nil), buf[:n]...)}, sentAt: at})
		seq++
	}
	length := time.Duration(len(s)+50) * 20 * time.Millisecond
	for _, loss := range []float64{0, 0.05, 0.10} {
		dec, _ := opus.NewDecoder()
		b := New(dec, Config{})
		frames := runPCM(b, through(append([]sent(nil), s...), netsim.New(loss, 40*time.Millisecond, 0, 11)), length)
		c, worst, _ := pcm.Segmental(src, frames, opus.SampleRate, opus.SampleRate/2)
		st := b.Stats()
		t.Logf("loss %2.0f%%, jitter 40 ms: correlation %.3f (worst second %.3f); lost %d (FEC %d, PLC %d), late %d, target %v",
			loss*100, c, worst, st.Lost, st.FEC, st.PLC, st.Late, st.Target)
		if c < 0.8 {
			t.Errorf("correlation %.3f at %.0f%% loss", c, loss*100)
		}
	}
}

func runPCM(b *Buffer, s []sent, d time.Duration) []float32 {
	var all []float32
	out := make([]float32, 480)
	i := 0
	for t := t0; t.Before(t0.Add(d)); t = t.Add(10 * time.Millisecond) {
		for ; i < len(s) && !s[i].at.After(t); i++ {
			if !s[i].drop {
				b.Push(s[i].pkt, s[i].at)
			}
		}
		b.Pull(out)
		all = append(all, out...)
	}
	return all
}

func speechFixture(t *testing.T) []float32 {
	path := filepath.Join("..", "..", ".dev", "fixtures", "speech.wav")
	f, err := os.Open(path)
	if err != nil {
		t.Skip("no speech fixture: run scripts/voice-fixtures.sh")
	}
	defer f.Close()
	data, _, err := pcm.ReadWAV(f)
	if err != nil {
		t.Fatal(err)
	}
	return pcm.F32FromS16(data)
}

// TestPlaying: the position a pull began at is the RTP timestamp of the
// first sample it took (lip sync holds video against it).
func TestPlaying(t *testing.T) {
	b := New(fakeDec{}, Config{})
	if _, _, ok := b.Playing(); ok {
		t.Fatal("playing before anything came")
	}
	now := time.Now()
	const ts0 = 0xfffff000 // across the wrap
	for k := range 5 {
		b.Push(Packet{Seq: uint16(k), TS: ts0 + uint32(k*opus.FrameSamples), Payload: payload(k)}, now)
	}
	out := make([]float32, 480)
	for k := range 7 {
		b.Pull(out)
		ts, at, ok := b.Playing()
		if want := uint32(ts0 + k*480); !ok || ts != want || time.Since(at) > time.Second {
			t.Fatalf("pull %d: playing %d (%v), want %d", k, ts, ok, want)
		}
	}
}
