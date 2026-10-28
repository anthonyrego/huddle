package opus

import (
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/anthonyrego/huddle/internal/pcm"
)

// speech is the speech fixture (scripts/voice-fixtures.sh), or a voice-like
// synthetic signal when it hasn't been made: harmonics of a gliding pitch,
// syllable envelopes and pauses.
func speech(tb testing.TB) (samples []float32, real bool) {
	tb.Helper()
	path := os.Getenv("HUDDLE_SPEECH_WAV")
	if path == "" {
		path = filepath.Join("..", "..", ".dev", "fixtures", "speech.wav")
	}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		data, rate, err := pcm.ReadWAV(f)
		if err == nil && rate == SampleRate {
			return pcm.F32FromS16(data), true
		}
	}
	return synthSpeech(10 * SampleRate), false
}

func synthSpeech(n int) []float32 {
	out := make([]float32, n)
	phase := 0.0
	for i := range out {
		t := float64(i) / SampleRate
		f0 := 140 + 30*math.Sin(2*math.Pi*0.7*t)
		phase += 2 * math.Pi * f0 / SampleRate
		var v float64
		for h := 1; h <= 12; h++ {
			v += math.Sin(float64(h)*phase) / float64(h)
		}
		syll := math.Max(0, math.Sin(2*math.Pi*4*t))      // ~4 syllables a second
		words := 0.5+0.5*math.Sin(2*math.Pi*0.25*t) > 0.3 // with pauses
		if !words {
			syll = 0
		}
		out[i] = float32(0.2 * v * syll)
	}
	return out
}

// encodeAll encodes s in 20 ms frames (a DTX frame is kept as sent: the
// tests drop it themselves when they want to).
func encodeAll(tb testing.TB, s []float32, o Options) [][]byte {
	tb.Helper()
	enc, err := NewEncoder(o)
	if err != nil {
		tb.Fatal(err)
	}
	defer enc.Close()
	var pkts [][]byte
	buf := make([]byte, MaxPacket)
	for i := 0; i+FrameSamples <= len(s); i += FrameSamples {
		n, err := enc.Encode(s[i:i+FrameSamples], buf)
		if err != nil {
			tb.Fatal(err)
		}
		pkts = append(pkts, append([]byte(nil), buf[:n]...))
	}
	return pkts
}

func TestRoundTrip(t *testing.T) {
	src, _ := speech(t)
	pkts := encodeAll(t, src, Options{})
	for _, b := range []Backend{Libopus, Pion} {
		dec, err := NewDecoderWith(b)
		if err != nil {
			t.Fatal(err)
		}
		out := decodeAll(t, dec, pkts, nil, false)
		lag, c := pcm.BestLag(src, out, 2000)
		t.Logf("%s: %d packets, lag %d samples, correlation %.3f", b, len(pkts), lag, c)
		if c < 0.9 {
			t.Errorf("%s: decoded correlation %.3f, want ≥ 0.9", b, c)
		}
	}
}

func TestDTX(t *testing.T) {
	enc, err := NewEncoder(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	silence, buf := make([]float32, FrameSamples), make([]byte, MaxPacket)
	dtx := 0
	for range 100 { // 2 s of silence
		n, err := enc.Encode(silence, buf)
		if err != nil {
			t.Fatal(err)
		}
		if IsDTX(n) {
			dtx++
		}
	}
	if dtx < 80 {
		t.Fatalf("%d of 100 silent frames were DTX, want most", dtx)
	}
	if _, err := NewEncoder(Options{NoDTX: true}); err != nil {
		t.Fatal(err)
	}
}

func TestFECRecovers(t *testing.T) {
	src, _ := speech(t)
	pkts := encodeAll(t, src, Options{})
	dec, _ := NewDecoder()
	ref := decodeAll(t, dec, pkts, nil, false)
	lost := lossPattern(len(pkts), 0.10, 7)
	for _, fec := range []bool{true, false} {
		dec, _ := NewDecoder()
		out := decodeAll(t, dec, pkts, lost, fec)
		c := lostFrameCorrelation(ref, out, lost)
		t.Logf("libopus fec=%v: correlation on lost frames %.3f", fec, c)
		if fec && c < 0.5 {
			t.Errorf("FEC-recovered frames correlate %.3f with the clean decode, want ≥ 0.5", c)
		}
	}
}

// decodeAll decodes pkts, treating lost[i] as never received and DTX
// frames as not sent (silence). With fec, a lost frame is recovered from
// the next packet when that one arrived.
func decodeAll(tb testing.TB, dec Decoder, pkts [][]byte, lost []bool, fec bool) []float32 {
	tb.Helper()
	defer dec.Close()
	out := make([]float32, 0, len(pkts)*FrameSamples)
	frame := make([]float32, FrameSamples*6)
	for i, p := range pkts {
		var n int
		var err error
		switch {
		case lost != nil && lost[i]:
			if fec && i+1 < len(pkts) && !lost[i+1] && !IsDTX(len(pkts[i+1])) {
				n, err = dec.DecodeFEC(pkts[i+1], frame[:FrameSamples])
			} else {
				n, err = dec.Conceal(frame[:FrameSamples])
			}
		case IsDTX(len(p)):
			clear(frame[:FrameSamples])
			n = FrameSamples
		default:
			n, err = dec.Decode(p, frame)
		}
		if err != nil {
			tb.Fatalf("packet %d: %v", i, err)
		}
		out = append(out, frame[:n]...)
	}
	return out
}

func lossPattern(n int, p float64, seed uint64) []bool {
	r := rand.New(rand.NewPCG(seed, seed))
	lost := make([]bool, n)
	for i := range lost {
		lost[i] = r.Float64() < p
	}
	return lost
}

// lostFrameCorrelation compares only the frames that were lost (and so
// made up) with the clean decode.
func lostFrameCorrelation(ref, out []float32, lost []bool) float64 {
	var a, b []float32
	for i, l := range lost {
		if l && (i+1)*FrameSamples <= min(len(ref), len(out)) {
			a = append(a, ref[i*FrameSamples:(i+1)*FrameSamples]...)
			b = append(b, out[i*FrameSamples:(i+1)*FrameSamples]...)
		}
	}
	return pcm.Correlation(a, b, 0)
}

// TestConcealmentCompared is the FEC/PLC comparison AGENTS.md records:
// each backend's decode of the speech fixture under 5% and 10% random
// loss, scored as correlation with the source (whole signal) and with the
// clean decode (lost frames only). Run with -v to see the table.
func TestConcealmentCompared(t *testing.T) {
	src, real := speech(t)
	if !real {
		t.Log("no speech fixture (scripts/voice-fixtures.sh): using synthetic speech")
	}
	pkts := encodeAll(t, src, Options{})
	type mode struct {
		name string
		b    Backend
		fec  bool
	}
	modes := []mode{{"libopus FEC+PLC", Libopus, true}, {"libopus PLC", Libopus, false}, {"pion repeat-fade", Pion, false}}
	for _, loss := range []float64{0.05, 0.10} {
		lost := lossPattern(len(pkts), loss, 42)
		for _, m := range modes {
			d0, _ := NewDecoderWith(m.b)
			ref := decodeAll(t, d0, pkts, nil, false)
			d1, _ := NewDecoderWith(m.b)
			out := decodeAll(t, d1, pkts, lost, m.fec)
			_, whole := pcm.BestLag(src, out, 2000)
			t.Logf("loss %2.0f%%  %-17s  source corr %.3f  lost-frame corr %.3f", loss*100, m.name, whole, lostFrameCorrelation(ref, out, lost))
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	src, _ := speech(b)
	enc, err := NewEncoder(Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	buf := make([]byte, MaxPacket)
	frames := len(src) / FrameSamples
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := i % frames
		if _, err := enc.Encode(src[f*FrameSamples:(f+1)*FrameSamples], buf); err != nil {
			b.Fatal(err)
		}
	}
	reportRealtime(b)
}

func BenchmarkDecode(b *testing.B) {
	src, _ := speech(b)
	pkts := encodeAll(b, src, Options{NoDTX: true})
	for _, be := range []Backend{Libopus, Pion} {
		b.Run(string(be), func(b *testing.B) {
			dec, err := NewDecoderWith(be)
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Close()
			out := make([]float32, FrameSamples*6)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := dec.Decode(pkts[i%len(pkts)], out); err != nil {
					b.Fatal(err)
				}
			}
			reportRealtime(b)
		})
	}
}

func BenchmarkConceal(b *testing.B) {
	for _, be := range []Backend{Libopus, Pion} {
		b.Run(string(be), func(b *testing.B) {
			dec, _ := NewDecoderWith(be)
			defer dec.Close()
			src, _ := speech(b)
			pkts := encodeAll(b, src[:SampleRate], Options{NoDTX: true})
			out := make([]float32, FrameSamples*6)
			for _, p := range pkts {
				dec.Decode(p, out)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dec.Conceal(out[:FrameSamples])
			}
			reportRealtime(b)
		})
	}
}

// reportRealtime adds the share of one core a 20 ms stream takes.
func reportRealtime(b *testing.B) {
	perFrame := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	b.ReportMetric(100*perFrame/20e6, "%core")
}

// TestConcealUnderGC is the regression test for the moving-stack bug (see
// pin.go): concealment-heavy decoding while the GC runs as often as it can
// and decoders come and go. Without the pinned worker it aborts inside
// libopus within a few hundred packets.
func TestConcealUnderGC(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	src, _ := speech(t)
	pkts := encodeAll(t, src[:min(len(src), 8*SampleRate)], Options{})
	lost := lossPattern(len(pkts), 0.10, 42)
	for i := range 4 {
		dec, err := NewDecoder()
		if err != nil {
			t.Fatal(err)
		}
		decodeAll(t, dec, pkts, lost, i%2 == 0)
		runtime.GC()
	}
}

func TestPacketSamples(t *testing.T) {
	src, _ := speech(t)
	for _, p := range encodeAll(t, src[:SampleRate], Options{}) {
		if n := PacketSamples(p); n != FrameSamples {
			t.Fatalf("packet %x…: %d samples, want %d", p[:min(len(p), 2)], n, FrameSamples)
		}
	}
	for _, c := range []struct {
		p    []byte
		want int
	}{{[]byte{0x00}, 480}, {[]byte{0x0b << 3}, 2880}, {[]byte{31<<3 | 1}, 1920}, {[]byte{16<<3 | 3, 3}, 360}, {nil, 0}} {
		if got := PacketSamples(c.p); got != c.want {
			t.Errorf("PacketSamples(%x) = %d, want %d", c.p, got, c.want)
		}
	}
}
