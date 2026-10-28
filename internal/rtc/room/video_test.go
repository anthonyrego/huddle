package room

import (
	"bytes"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/anthonyrego/huddle/internal/video"
)

// units are made-up access units: a keyframe (SPS, PPS, a big IDR slice
// that takes several FU-A packets) every keyEvery, small slices between.
func units(n, keyEvery int) [][]byte {
	var out [][]byte
	for i := range n {
		fill := func(t byte, size int) []byte {
			b := make([]byte, size)
			b[0] = t
			for j := 1; j < size; j++ {
				b[j] = byte(i + j) // no zeros: no start codes inside
				if b[j] == 0 {
					b[j] = 1
				}
			}
			return b
		}
		if i%keyEvery == 0 {
			out = append(out, video.AppendAnnexB(nil, fill(0x67, 12), fill(0x68, 4), fill(0x65, 5000)))
		} else {
			out = append(out, video.AppendAnnexB(nil, fill(0x41, 300+i%7*400)))
		}
	}
	return out
}

// packetize is what lksdk's WriteSample does: one packetizer, 1200-byte
// packets, the marker on each unit's last.
func packetize(aus [][]byte) []*rtp.Packet {
	pz := rtp.NewPacketizer(1200, 96, 1234, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	var out []*rtp.Packet
	for _, a := range aus {
		out = append(out, pz.Packetize(a, 3000)...)
	}
	return out
}

// same reports whether two units hold the same NAL units.
func same(a, b []byte) bool {
	na, nb := video.SplitAnnexB(a), video.SplitAnnexB(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		if !bytes.Equal(na[i], nb[i]) {
			return false
		}
	}
	return true
}

func TestAssembleInOrder(t *testing.T) {
	aus := units(40, 10)
	a := newAssembler()
	now := time.Now()
	var got []au
	for _, p := range packetize(aus) {
		got = append(got, a.push(p, now)...)
	}
	// The first unit is skipped: the assembler starts after a marker.
	if len(got) != len(aus)-1 {
		t.Fatalf("%d units out, want %d", len(got), len(aus)-1)
	}
	for i, u := range got {
		if !same(u.data, aus[i+1]) {
			t.Fatalf("unit %d differs", i+1)
		}
		if u.key != ((i+1)%10 == 0) || u.gap {
			t.Fatalf("unit %d: key %v gap %v", i+1, u.key, u.gap)
		}
	}
}

func TestAssembleReorderAndLate(t *testing.T) {
	aus := units(30, 10)
	pkts := packetize(aus)
	// Swap neighbours here and there; a reordered packet is a hole for a
	// moment, never a loss.
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 1; i+1 < len(pkts); i++ {
		if rng.IntN(5) == 0 {
			pkts[i], pkts[i+1] = pkts[i+1], pkts[i]
			i++
		}
	}
	a := newAssembler()
	now := time.Now()
	var got []au
	for _, p := range pkts {
		got = append(got, a.push(p, now)...)
	}
	if len(got) < len(aus)-2 {
		t.Fatalf("%d units out of %d", len(got), len(aus))
	}
	for _, u := range got {
		if u.gap {
			t.Fatal("a reorder read as a loss")
		}
	}
}

func TestAssembleLoss(t *testing.T) {
	aus := units(30, 10)
	pkts := packetize(aus)
	// Lose a packet of unit 4's keyframe-less slice... find the packets of
	// unit 5 (a P slice) and drop its only packet.
	var lost uint16
	n := 0
	for i, p := range pkts {
		if p.Marker {
			n++
			if n == 6 { // the 6th marker ends unit 5
				lost = p.SequenceNumber
				pkts = append(pkts[:i:i], pkts[i+1:]...)
				break
			}
		}
	}
	a := newAssembler()
	start := time.Now()
	var got []au
	for i, p := range pkts {
		// 10 ms a packet: the hole waits lossWait, then is given up.
		got = append(got, a.push(p, start.Add(time.Duration(i)*10*time.Millisecond))...)
	}
	gaps := 0
	for _, u := range got {
		if u.gap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("%d gaps after losing packet %d, want 1", gaps, lost)
	}
	// A retransmission in time is no loss.
	pkts = packetize(aus)
	var held *rtp.Packet
	a = newAssembler()
	got = got[:0]
	for i, p := range pkts {
		if i == 20 {
			held = p
			continue
		}
		got = append(got, a.push(p, start)...)
		if i == 25 {
			got = append(got, a.push(held, start.Add(50*time.Millisecond))...)
		}
	}
	for _, u := range got {
		if u.gap {
			t.Fatal("a retransmission that came in time read as a loss")
		}
	}
	if len(got) != len(aus)-1 {
		t.Fatalf("%d units with a retransmission, want %d", len(got), len(aus)-1)
	}
}

func TestAssemblePadding(t *testing.T) {
	aus := units(10, 5)
	pkts := packetize(aus)
	// The SFU's padding between units takes sequence numbers.
	var out []*rtp.Packet
	shift := uint16(0)
	for _, p := range pkts {
		p.SequenceNumber += shift
		out = append(out, p)
		if p.Marker {
			out = append(out, &rtp.Packet{Header: rtp.Header{SequenceNumber: p.SequenceNumber + 1, Timestamp: p.Timestamp, Padding: true}})
			shift++
		}
	}
	a := newAssembler()
	var got []au
	for _, p := range out {
		got = append(got, a.push(p, time.Now())...)
	}
	if len(got) != len(aus)-1 {
		t.Fatalf("%d units with padding, want %d", len(got), len(aus)-1)
	}
}

func TestAllocate(t *testing.T) {
	cam, scr := &videoPub{}, &videoPub{}
	var a allocator
	now := time.Now()
	a.add(cam, []int{1_700_000, 450_000, 160_000}, now)
	// 1.2 Mbps from the start: 180p and 360p at once, not 720p.
	a.allocate(1_200_000, 0, now)
	if got := a.bitrates(cam, 3); got[0] != 0 || got[1] != 450_000 || got[2] != 160_000 {
		t.Fatalf("at 1.2 Mbps: %v", got)
	}
	// GCC climbs to 2.5 Mbps and has never dropped: 720p comes after a
	// short probe (360p padded up by 720p's floor over probeFirst).
	var on time.Time
	for i := 1; i <= 20 && on.IsZero(); i++ {
		at := now.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(2_500_000, 650_000+min(i, 9)*80_000, at)
		if got := a.bitrates(cam, 3); got[0] > 0 {
			on = at
			if got[0] != 680_000 {
				t.Fatalf("720p back at %d, not its floor", got[0])
			}
		}
	}
	if d := on.Sub(now); on.IsZero() || d < probeFirst {
		t.Fatalf("720p after %v", d)
	}
	// Once the cap counts it, it grows to its full rate.
	now = on.Add(freeFor + time.Second)
	a.allocate(2_500_000, 2_000_000, now)
	if got := a.bitrates(cam, 3); got[0] != 1_700_000 {
		t.Fatalf("at 2.5 Mbps, sending 2 Mbps: %v", got)
	}
	// Congestion: GCC drops to 900 kbps. 720p pauses, the rest stay.
	paused := now.Add(2 * time.Second)
	a.allocate(900_000, 1_800_000, paused)
	if got := a.bitrates(cam, 3); got[0] != 0 || got[1] == 0 || got[2] == 0 {
		t.Fatalf("at 0.9 Mbps: %v", got)
	}
	// GCC climbs again while less is sent: after its hold (15 s) and calm,
	// a probe overdrives 360p, rising towards 720p's floor; 720p stays off.
	var began time.Time
	for i := 1; i <= 60 && began.IsZero(); i++ {
		at := paused.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(3_000_000, 650_000, at)
		if got := a.bitrates(cam, 3); got[1] > 450_000 {
			began = at
			if d := at.Sub(paused); d < holdFirst {
				t.Fatalf("a probe %v after the pause, before its hold", d)
			}
			if got[0] != 0 {
				t.Fatalf("720p on as the probe begins: %v", got)
			}
		}
	}
	if began.IsZero() {
		t.Fatal("no probe")
	}
	a.allocate(3_000_000, 900_000, began.Add(probeFor/2))
	if got := a.bitrates(cam, 3); got[0] != 0 || got[1] < 450_000+300_000 || got[1] > 450_000+400_000 {
		t.Fatalf("halfway through the probe: %v", got)
	}
	// The probe fails: GCC drops below what it adds. The boost goes at
	// once, and the next hold is twice as long.
	failed := began.Add(probeFor * 3 / 4)
	a.allocate(1_000_000, 900_000, failed)
	if got := a.bitrates(cam, 3); got[0] != 0 || got[1] != 450_000 {
		t.Fatalf("after a failed probe: %v", got)
	}
	var again time.Time
	for i := 1; i <= 240 && again.IsZero(); i++ {
		at := failed.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(3_000_000, 650_000, at)
		if got := a.bitrates(cam, 3); got[1] > 450_000 && again.IsZero() {
			again = at
			if d := at.Sub(failed); d < 2*holdFirst {
				t.Fatalf("a probe %v after a failed one, before its doubled hold", d)
			}
		}
	}
	// This one passes: what was sent rose by the floor without a drop, and
	// 720p comes back at its floor.
	on = time.Time{}
	for i := 1; i <= 30 && on.IsZero(); i++ {
		at := again.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(3_000_000+i*10_000, 650_000+min(i, 16)*45_000, at)
		if got := a.bitrates(cam, 3); got[0] > 0 {
			on = at
			if got[0] != 680_000 || got[1] != 450_000 {
				t.Fatalf("after a probe that passed: %v", got)
			}
		}
	}
	if on.IsZero() {
		t.Fatal("a probe that should pass didn't")
	}
	// The cap: 1.5× what's sent (plus slack).
	usable := a.allocate(3_000_000, 1_000_000, now.Add(5*time.Minute))
	if usable != 1_600_000 {
		t.Fatalf("capped estimate %d", usable)
	}
	// Two videos share it lowest first: the screen's half size (800 kbps)
	// only after the camera's 360p.
	a.add(scr, []int{2_500_000, 800_000}, now.Add(6*time.Minute))
	at := now.Add(6 * time.Minute)
	a.allocate(1_000_000, 2_000_000, at)
	c, s := a.bitrates(cam, 3), a.bitrates(scr, 2)
	if c[0] != 0 || c[1] != 450_000 || c[2] != 160_000 || s[0] != 0 || s[1] != 0 {
		t.Fatalf("shared at 1 Mbps: camera %v screen %v", c, s)
	}
	// The drop to 1 Mbps was just now: no probe for the screen's half size
	// before calm; then one, overdriving the camera's 360p, and the screen
	// comes on when it passes.
	a.allocate(1_500_000, 610_000, at.Add(time.Second))
	if c, s = a.bitrates(cam, 3), a.bitrates(scr, 2); s[1] != 0 || c[1] != 450_000 {
		t.Fatalf("a probe before calm: camera %v screen %v", c, s)
	}
	start := at.Add(calm + time.Second)
	for i := range 30 {
		a.allocate(1_500_000, 610_000+min(i, 16)*20_000, start.Add(time.Duration(i)*500*time.Millisecond))
		if s = a.bitrates(scr, 2); s[1] > 0 {
			break
		}
	}
	c, s = a.bitrates(cam, 3), a.bitrates(scr, 2)
	if c[0] != 0 || c[1] != 450_000 || s[1] != 320_000 || s[0] != 0 {
		t.Fatalf("shared at 1.5 Mbps: camera %v screen %v", c, s)
	}
	a.remove(scr)
	if len(a.layers) != 3 {
		t.Fatalf("%d layers after removing the screen", len(a.layers))
	}
}

// writes records what went out, and when.
type writes struct {
	mu   sync.Mutex
	ssrc []uint32
	at   []time.Time
	n    int
}

func (w *writes) Write(h *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ssrc = append(w.ssrc, h.SSRC)
	w.at = append(w.at, time.Now())
	w.n += h.MarshalSize() + len(p)
	return len(p), nil
}

func TestPacerAudioFirst(t *testing.T) {
	p := newPacer(400_000) // 600 kbps of video
	defer p.Close()
	var w writes
	p.stream(&interceptor.StreamInfo{SSRC: 1, MimeType: "audio/opus"})
	p.stream(&interceptor.StreamInfo{SSRC: 2, MimeType: "video/H264", SSRCRetransmission: 3})
	p.AddStream(1, &w)
	p.AddStream(2, &w)
	// A 60 KB keyframe, then a voice packet: the voice goes at once.
	payload := make([]byte, 1200)
	start := time.Now()
	for range 50 {
		p.Write(&rtp.Header{SSRC: 2}, payload, nil)
	}
	p.Write(&rtp.Header{SSRC: 1}, payload[:100], nil)
	if _, err := p.Write(&rtp.Header{SSRC: 3}, payload, nil); err != nil {
		t.Fatalf("a retransmission: %v", err)
	}
	if _, err := p.Write(&rtp.Header{SSRC: 9}, payload, nil); err == nil {
		t.Fatal("an unknown stream was taken")
	}
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	audioAt := -1
	for i, s := range w.ssrc {
		if s == 1 {
			audioAt = i
		}
	}
	sentEarly := len(w.ssrc)
	w.mu.Unlock()
	if audioAt < 0 || audioAt > 3 {
		t.Fatalf("the voice went out %dth", audioAt)
	}
	// The rest drain at paceFactor × 400 kbps: 61 KB in about a second.
	if sentEarly > 20 {
		t.Fatalf("%d packets out in 50 ms: not paced", sentEarly)
	}
	for time.Since(start) < 3*time.Second {
		w.mu.Lock()
		n := len(w.ssrc)
		w.mu.Unlock()
		if n == 52 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(start)
	if took < 600*time.Millisecond || took > 1300*time.Millisecond {
		t.Fatalf("drained in %v, want about a second", took)
	}
	if r := p.sentRate(); r < 400_000 {
		t.Fatalf("sent rate %d", r)
	}
}

func TestSRClock(t *testing.T) {
	var c srClock
	if _, ok := c.at(0, 90000); ok {
		t.Fatal("a mapping before any sender report")
	}
	// 3600 s + 0.5 s after 1900, at RTP 1000.
	c.read(&rtcp.SenderReport{NTPTime: 3600<<32 | 1<<31, RTPTime: 1000})
	at, _ := c.at(1000+90000, 90000)
	if want := 3601500 * time.Millisecond; at != want {
		t.Fatalf("one second of 90 kHz later: %v, want %v", at, want)
	}
	// Across the wrap.
	c.read(&rtcp.SenderReport{NTPTime: 3600 << 32, RTPTime: 0xffffff00})
	at, _ = c.at(0x100, 48000)
	if want := 3600*time.Second + 512*time.Second/48000; at != want {
		t.Fatalf("across the wrap: %v, want %v", at, want)
	}
}

// TestFiller: a probe's filler is one SEI NAL unit that survives
// packetizing and reassembly, and the unit still decodes as before (the
// decoder takes only parameter sets and slices).
func TestFiller(t *testing.T) {
	aus := units(6, 3)
	padded := make([][]byte, len(aus))
	for i, a := range aus {
		padded[i] = appendFiller(append([]byte(nil), a...), 3000+i*255)
	}
	for i, p := range padded {
		nals := video.SplitAnnexB(p)
		last := nals[len(nals)-1]
		if video.NALType(last) != nalSEI || last[1] != seiFillerPayload {
			t.Fatalf("unit %d: the filler isn't an SEI filler payload", i)
		}
		for j := 0; j+2 < len(last); j++ {
			if last[j] == 0 && last[j+1] == 0 {
				t.Fatalf("unit %d: zeros in the filler", i)
			}
		}
	}
	a := newAssembler()
	var got []au
	for _, pk := range packetize(padded) {
		got = append(got, a.push(pk, time.Now())...)
	}
	if len(got) != len(padded)-1 {
		t.Fatalf("%d units out of %d", len(got), len(padded))
	}
	for i, u := range got {
		if !same(u.data, padded[i+1]) {
			t.Fatalf("unit %d changed on the way", i+1)
		}
	}
}

// TestSqueeze: a layer that just came back at its floor rides out one
// overuse cut from GCC (to 0.85 of what arrived) instead of pausing.
func TestSqueeze(t *testing.T) {
	scr := &videoPub{}
	var a allocator
	now := time.Now()
	a.add(scr, []int{2_500_000, 800_000}, now)
	a.allocate(1_200_000, 0, now) // half size at once, full size waits
	var on time.Time
	for i := 1; i <= 30 && on.IsZero(); i++ {
		at := now.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(2_600_000, 840_000+min(i, 9)*120_000, at)
		if got := a.bitrates(scr, 2); got[0] > 0 {
			on = at
		}
	}
	if on.IsZero() {
		t.Fatal("the full size never came")
	}
	cut := int(0.85 * float64(800_000+1_000_000+audioReserve))
	a.allocate(cut, 1_840_000, on.Add(300*time.Millisecond))
	if got := a.bitrates(scr, 2); got[0] == 0 || got[1] != 800_000 {
		t.Fatalf("after one cut to %d: %v", cut, got)
	}
	a.allocate(1_000_000, 1_500_000, on.Add(time.Second))
	if got := a.bitrates(scr, 2); got[0] != 0 {
		t.Fatalf("at 1 Mbps: %v", got)
	}
}
