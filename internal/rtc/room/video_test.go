package room

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

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
	// The stream starts with a keyframe's first packet: it comes out.
	if len(got) != len(aus) {
		t.Fatalf("%d units out, want %d", len(got), len(aus))
	}
	for i, u := range got {
		if !same(u.data, aus[i]) {
			t.Fatalf("unit %d differs", i)
		}
		if u.key != (i%10 == 0) || u.gap {
			t.Fatalf("unit %d: key %v gap %v", i, u.key, u.gap)
		}
	}
}

// TestAssembleJoinMidway: joined in the middle of a unit, the assembler
// starts after the first marker, or at a keyframe's first packet if one
// comes before it.
func TestAssembleJoinMidway(t *testing.T) {
	aus := units(30, 10)
	pkts := packetize(aus)
	from := func(unit, packet int) []*rtp.Packet {
		n := 0
		for i, p := range pkts {
			if n == unit {
				return pkts[i+packet:]
			}
			if p.Marker {
				n++
			}
		}
		return nil
	}
	for _, c := range []struct {
		name         string
		unit, packet int
		first        int // the first unit out
	}{
		{"in a P slice", 6, 1, 7},  // unit 6 takes three packets
		{"in a keyframe", 0, 2, 1}, // its IDR is in FU-A fragments
		{"at a keyframe", 10, 0, 10},
	} {
		a := newAssembler()
		var got []au
		for _, p := range from(c.unit, c.packet) {
			got = append(got, a.push(p, time.Now())...)
		}
		if len(got) != len(aus)-c.first || !same(got[0].data, aus[c.first]) || got[0].gap {
			t.Fatalf("joined %s: %d units out, want %d from unit %d", c.name, len(got), len(aus)-c.first, c.first)
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
	if len(got) != len(aus) {
		t.Fatalf("%d units with a retransmission, want %d", len(got), len(aus))
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
	if len(got) != len(aus) {
		t.Fatalf("%d units with padding, want %d", len(got), len(aus))
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

// TestSyncClocks: a camera and a voice from one sender, through a network
// that adds 0–40 ms. Frames are timed against the voice by arrivals, the
// jitter and the encoder's slower frames gone; before any packet of one,
// by sender reports, which (as pion stamps them) place a frame as late as
// the encoder was with the frame before.
func TestSyncClocks(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	t0 := time.Unix(5000, 0)
	const ntpOffset = 70 * 365 * 24 * time.Hour // the sender's clock: since 1900, near enough
	baseV, baseA := uint32(0xffff0000), uint32(0x7fffff00)
	type arrival struct {
		at    time.Time
		video bool
		ts    uint32
	}
	var arr []arrival
	const net = 2 * time.Millisecond
	jit := func(d time.Duration) time.Duration { return time.Duration(rng.Int64N(int64(d))) }
	encoded := map[int]time.Duration{} // each frame's encoding: 5–13 ms
	for n := range 90 {                // 3 s of 30 fps, four packets a frame
		c := t0.Add(time.Duration(n) * time.Second / 30)
		encoded[n] = 5*time.Millisecond + jit(8*time.Millisecond)
		for k := range 4 {
			arr = append(arr, arrival{c.Add(encoded[n] + net + time.Duration(k)*time.Millisecond + jit(40*time.Millisecond)), true, baseV + uint32(n*3000)})
		}
	}
	for k := range 150 { // 3 s of 20 ms packets, each sent once captured
		sent := t0.Add(time.Duration(k+1)*20*time.Millisecond + 500*time.Microsecond)
		arr = append(arr, arrival{sent.Add(net + jit(40*time.Millisecond)), false, baseA + uint32(k*960)})
	}
	slices.SortFunc(arr, func(x, y arrival) int { return x.at.Compare(y.at) })
	vts := baseV + 60*3000                 // the frame taken 2 s in
	ats := baseA + uint32(48000*1950/1000) // the voice's sample taken 50 ms before it
	// 50 ms apart, the frame placed late by its encoding, the voice by its
	// packet and its encoding, less the packet voiceSent takes off.
	want := func(encode time.Duration) time.Duration {
		return 50*time.Millisecond + encode - 20*time.Millisecond - 500*time.Microsecond + voiceSent
	}
	// Sender reports as pion's sender interceptor makes them: the last
	// packet's timestamp at the time it was written.
	ntp := func(at time.Time) uint64 {
		d := at.Sub(time.Unix(0, 0)) + ntpOffset
		return uint64(d/time.Second)<<32 | uint64(d%time.Second)<<32/uint64(time.Second)
	}
	vsr := &rtcp.SenderReport{NTPTime: ntp(t0.Add(59*time.Second/30 + encoded[59])), RTPTime: baseV + 59*3000}
	asr := &rtcp.SenderReport{NTPTime: ntp(t0.Add(100*20*time.Millisecond + 500*time.Microsecond)), RTPTime: baseA + 99*960}

	var v, a srClock
	if _, _, ok := syncGap(&v, &a, vts, ats); ok {
		t.Fatal("a gap with nothing come yet")
	}
	v.read(vsr)
	a.read(asr)
	gap, si, ok := syncGap(&v, &a, vts, ats)
	if !ok || si.How != "reports" || durAbs(gap-want(encoded[59])) > 100*time.Microsecond {
		t.Fatalf("by reports alone: %v by %q (ok %v), want %v", gap, si.How, ok, want(encoded[59]))
	}
	v, a = srClock{}, srClock{}
	for _, x := range arr {
		if x.video {
			v.arrived(x.ts, x.at, 90000)
		} else {
			a.arrived(x.ts, x.at, 48000)
		}
	}
	// By arrivals, each track's least delay: the voice's after its
	// packet, the video's after its quickest encoding, with what's left of
	// the jitter (a few ms).
	least := func(video bool) time.Duration {
		d := time.Hour
		for _, x := range arr {
			switch {
			case x.video && video:
				d = min(d, x.at.Sub(t0)-time.Duration(int32(x.ts-baseV))*time.Second/90000)
			case !x.video && !video:
				d = min(d, x.at.Sub(t0)-time.Duration(int32(x.ts-baseA))*time.Second/48000)
			}
		}
		return d
	}
	byArrivals := 50*time.Millisecond + least(true) - least(false) + voiceSent
	gap, si, ok = syncGap(&v, &a, vts, ats)
	if !ok || si.How != "arrivals" || durAbs(gap-byArrivals) > 100*time.Microsecond || durAbs(gap-want(5*time.Millisecond)) > 3*time.Millisecond {
		t.Fatalf("by arrivals: %v by %q (ok %v), want %v (%v with no jitter)", gap, si.How, ok, byArrivals, want(5*time.Millisecond))
	}
	v.read(vsr)
	a.read(asr)
	if gap2, si, _ := syncGap(&v, &a, vts, ats); si.How != "arrivals" || gap2 != gap || durAbs(si.Reports-want(encoded[59])) > 100*time.Microsecond {
		t.Fatalf("with reports too: %v by %q, reports %v", gap2, si.How, si.Reports)
	}
	// LiveKit's made-up silence doesn't count as the voice's arrival.
	silence := append([]byte{0xf8, 0xff, 0xfe}, make([]byte, 77)...)
	if !madeUpSilence(silence) || madeUpSilence([]byte{0xf8, 0xff, 0xfe}) || madeUpSilence(append([]byte{0xf8, 0xff, 0xfe, 1}, make([]byte, 76)...)) {
		t.Fatal("LiveKit's silent frame read wrong")
	}
}

func durAbs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
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
	if len(got) != len(padded) {
		t.Fatalf("%d units out of %d", len(got), len(padded))
	}
	for i, u := range got {
		if !same(u.data, padded[i]) {
			t.Fatalf("unit %d changed on the way", i)
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
