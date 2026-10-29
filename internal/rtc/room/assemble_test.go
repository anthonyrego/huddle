package room

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/anthonyrego/huddle/internal/video"
)

// lossyStream is a 30 fps camera through a lossy network to an assembler
// and the read loop's keyframe rule, in simulated time: each packet is
// lost with the given chance, a lost one is resent (NACK) with the chance
// repair, 60–180 ms later, else never; every packet is held 0–80 ms more,
// in order (netsim's jitter). The sender makes a keyframe every 2 s and,
// asked (at most every 500 ms), at its next frame 100 ms or more later.
type lossyStream struct {
	loss, repair float64
	// The results, per frame put together (skipped or not): captured, its
	// last packet, put together, shown.
	frames []streamFrame
	// Shown is how many frames the decoder got; Asked the keyframes asked
	// for.
	Shown, Asked int
}

type streamFrame struct {
	captured, last, at time.Time
	shown, key         bool
}

// simUnit is a made-up access unit of about size bytes.
func simUnit(i int, key bool, size int) []byte {
	fill := func(t byte, n int) []byte {
		b := make([]byte, n)
		b[0] = t
		for j := 1; j < n; j++ {
			b[j] = byte(i+j) | 1 // no zeros: no start codes inside
		}
		return b
	}
	if key {
		return video.AppendAnnexB(nil, fill(0x67, 12), fill(0x68, 4), fill(0x65, size))
	}
	return video.AppendAnnexB(nil, fill(0x41, size))
}

func (s *lossyStream) run(t *testing.T, length time.Duration, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed+1))
	pz := rtp.NewPacketizer(1200, 102, 1, &codecs.H264Payloader{}, rtp.NewFixedSequencer(1), 90000)
	type arrival struct {
		at time.Time
		p  *rtp.Packet
	}
	start := time.Unix(1000, 0)
	const frame = time.Second / 30
	var lastInOrder time.Time
	capturedAt := map[uint32]time.Time{}
	// The sender and the receiver run together, frame by frame (the
	// receiver's keyframe requests change what's sent): before each frame
	// is sent, everything that has arrived by then is pushed.
	a := newAssembler()
	waitKey := true
	var lastAsk, keyDue time.Time
	var pending []arrival
	deliver := func(until time.Time) {
		slices.SortStableFunc(pending, func(x, y arrival) int { return x.at.Compare(y.at) })
		n := 0
		for _, ar := range pending {
			if ar.at.After(until) {
				break
			}
			n++
			for _, u := range a.push(ar.p, ar.at) {
				f := streamFrame{captured: capturedAt[u.ts], last: u.last, at: u.at, key: u.key}
				if u.gap {
					waitKey = true
				}
				if waitKey && !u.key {
					s.frames = append(s.frames, f)
					continue
				}
				waitKey = false
				f.shown = true
				s.Shown++
				s.frames = append(s.frames, f)
			}
			if (waitKey || a.awaitingKey()) && ar.at.Sub(lastAsk) >= pliEvery {
				lastAsk = ar.at
				s.Asked++
				if keyDue.IsZero() {
					keyDue = ar.at.Add(100 * time.Millisecond)
				}
			}
		}
		pending = pending[n:]
	}
	for i := 0; time.Duration(i)*frame < length; i++ {
		now := start.Add(time.Duration(i) * frame)
		deliver(now)
		key := i%60 == 0 || (!keyDue.IsZero() && !now.Before(keyDue))
		if key {
			keyDue = time.Time{}
		}
		size := 6000
		if key {
			size = 13000
		}
		ts := uint32(i * 3000)
		capturedAt[ts] = now
		for k, p := range pz.Packetize(simUnit(i, key, size), 3000) {
			p.Timestamp = ts
			sent := now.Add(time.Duration(k) * time.Millisecond)
			if rng.Float64() < s.loss {
				if rng.Float64() < s.repair {
					// The retransmission: out of order with what's around it.
					pending = append(pending, arrival{sent.Add(60*time.Millisecond + time.Duration(rng.Int64N(int64(120*time.Millisecond)))), p})
				}
				continue
			}
			at := sent.Add(time.Duration(rng.Int64N(int64(80 * time.Millisecond))))
			if at.Before(lastInOrder) {
				at = lastInOrder
			}
			lastInOrder = at
			pending = append(pending, arrival{at, p})
		}
	}
	deliver(start.Add(length + time.Second))
}

// TestAssembleHeavyLossKeepsUp: at 20% loss, with two in five lost
// packets never resent (what LiveKit does once a keyframe has been asked
// for), the assembler keeps up: each hole is waited for lossWait from when
// it showed, not from when the assembler got to it, so holes don't queue
// one wait behind another. Before, the frames fell seconds behind.
func TestAssembleHeavyLossKeepsUp(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		s := &lossyStream{loss: 0.2, repair: 0.6}
		s.run(t, 60*time.Second, seed)
		var early, late []time.Duration // put together after captured, first and last 20 s
		var worst time.Duration
		t0 := s.frames[0].captured
		for _, f := range s.frames {
			d := f.at.Sub(f.captured)
			switch since := f.captured.Sub(t0); {
			case since < 20*time.Second:
				early = append(early, d)
			case since >= 40*time.Second:
				late = append(late, d)
			}
			worst = max(worst, f.at.Sub(f.last))
		}
		if len(early) == 0 || len(late) == 0 {
			newest := s.frames[len(s.frames)-1]
			t.Fatalf("seed %d: falling behind: %d frames put together, the newest captured %v in, put together %v after", seed,
				len(s.frames), newest.captured.Sub(t0), newest.at.Sub(newest.captured))
		}
		e, l := median(early), median(late)
		t.Logf("seed %d: %d frames shown of %d put together, %d keyframes asked for; captured → put together median %v in the first 20 s, %v in the last; longest wait after a frame's last packet %v",
			seed, s.Shown, len(s.frames), s.Asked, e, l, worst)
		if l > e+100*time.Millisecond || l > 300*time.Millisecond {
			t.Fatalf("seed %d: falling behind: captured → put together median %v in the first 20 s, %v in the last", seed, e, l)
		}
		if worst > lossWait+50*time.Millisecond {
			t.Fatalf("seed %d: a frame waited %v after its last packet came", seed, worst)
		}
	}
}

// TestAssembleCut: a unit cut off before its marker (the SFU switched the
// viewer to another layer) is dropped, not glued onto what follows: at a
// keyframe's first packet, whether or not it has the cut unit's timestamp
// (a publication's layers share one clock), or at a packet with another
// timestamp, after which the decoder is told a unit was lost, and what
// isn't a keyframe is dropped until one comes.
func TestAssembleCut(t *testing.T) {
	aus := units(10, 10) // a keyframe, then P slices
	other := units(4, 4) // another layer from its keyframe
	for _, c := range []struct {
		name    string
		next    [][]byte // what follows the cut, sent on unbroken
		sameTS  bool
		gap     bool // the unit after the cut says one was lost
		skipped int  // units after the cut dropped while waiting for a keyframe
	}{
		{"a keyframe at the same time", other, true, false, 0},
		{"a later keyframe", other, false, false, 0},
		{"a later P slice, then a keyframe", append(slices.Clone(aus[6:]), units(1, 1)...), false, true, 4},
	} {
		pkts := packetize(aus[:6])
		pkts = pkts[:len(pkts)-1] // unit 5 takes two packets: its last never comes
		last := pkts[len(pkts)-1]
		rest := packetize(c.next)
		base := rest[0].Timestamp
		for i, p := range rest {
			p.SequenceNumber = last.SequenceNumber + 1 + uint16(i)
			p.Timestamp = last.Timestamp + (p.Timestamp - base)
			if !c.sameTS {
				p.Timestamp += 3000
			}
		}
		a := newAssembler()
		var got []au
		for _, p := range append(pkts, rest...) {
			got = append(got, a.push(p, time.Now())...)
		}
		want := append(append([][]byte(nil), aus[:5]...), c.next[c.skipped:]...)
		if len(got) != len(want) || a.Cut != 1 || a.Skipped != c.skipped {
			t.Fatalf("%s: %d units out, want %d; %d cut, %d skipped", c.name, len(got), len(want), a.Cut, a.Skipped)
		}
		for i, u := range got {
			if !same(u.data, want[i]) {
				t.Fatalf("%s: unit %d out differs", c.name, i)
			}
			if u.gap != (i == 5 && c.gap) {
				t.Fatalf("%s: unit %d out: gap %v", c.name, i, u.gap)
			}
		}
		if !got[5].key {
			t.Fatalf("%s: the first unit out after the cut isn't a keyframe", c.name)
		}
	}
}

// TestAssembleKeyframeAfterLoss: when the unit before a keyframe is lost
// for good, the keyframe still comes out: giving up the hole, the
// assembler picks up at the keyframe's first packet, not after its marker.
func TestAssembleKeyframeAfterLoss(t *testing.T) {
	aus := units(30, 10)
	pkts := packetize(aus)
	n := 0
	for i, p := range pkts {
		if p.Marker {
			if n == 9 { // unit 9's only packet
				pkts = append(pkts[:i:i], pkts[i+1:]...)
				break
			}
			n++
		}
	}
	a := newAssembler()
	start := time.Now()
	var got []au
	for i, p := range pkts {
		// 10 ms a packet: the hole waits lossWait, then is given up.
		got = append(got, a.push(p, start.Add(time.Duration(i)*10*time.Millisecond))...)
	}
	if len(got) != len(aus)-1 {
		t.Fatalf("%d units out, want %d", len(got), len(aus)-1)
	}
	for i, u := range got {
		want := i
		if i >= 9 {
			want++
		}
		if !same(u.data, aus[want]) {
			t.Fatalf("unit %d out isn't unit %d", i, want)
		}
		if u.gap != (want == 10) || u.key != (want%10 == 0) {
			t.Fatalf("unit %d: gap %v key %v", want, u.gap, u.key)
		}
	}
}

// TestAssembleSkipToKey: once a hole is given up, nothing waits on frames
// the decoder would throw away. A keyframe comes out as soon as it's
// whole, though a frame before it is still missing a packet; and one whose
// first packet comes late (resent) isn't taken for more of what's
// skipped.
func TestAssembleSkipToKey(t *testing.T) {
	aus := units(20, 10) // keyframes at 0 and 10
	type arrival struct {
		at time.Time
		p  *rtp.Packet
	}
	start := time.Unix(1000, 0)
	// Each unit's packets 1 ms apart, a unit every 33 ms.
	var plan []arrival
	var first []int // each unit's first packet in plan
	for i, a := range aus {
		first = append(first, len(plan))
		for k, p := range packetize([][]byte{a}) {
			p.SequenceNumber = uint16(len(plan))
			p.Timestamp = uint32(i * 3000)
			plan = append(plan, arrival{start.Add(time.Duration(i)*33*time.Millisecond + time.Duration(k)*time.Millisecond), p})
		}
	}
	// Unit 1 (one packet) is lost for good: the hole shows with unit 2 and
	// is given up lossWait later, before the keyframe at 10 comes; unit 2
	// goes with it (its start is unknown), 3 to 9 are skipped.
	for _, c := range []struct {
		name   string
		lose   []int                 // the packets lost, by plan index
		resend map[int]time.Duration // of those, resent this much later
		skip   int                   // units skipped
	}{
		// Unit 8 (one packet) is lost for good too: it isn't waited for.
		{"a hole before the keyframe", []int{first[1], first[8]}, nil, 6},
		// The keyframe's first packet is resent 60 ms after it was due.
		{"the keyframe's first packet late", []int{first[1], first[10]}, map[int]time.Duration{first[10]: 60 * time.Millisecond}, 7},
	} {
		lost := map[int]bool{}
		for _, i := range c.lose {
			lost[i] = true
		}
		var arr []arrival
		for i, a := range plan {
			if !lost[i] {
				arr = append(arr, a)
			} else if d, ok := c.resend[i]; ok {
				arr = append(arr, arrival{a.at.Add(d), a.p})
			}
		}
		slices.SortStableFunc(arr, func(x, y arrival) int { return x.at.Compare(y.at) })
		a := newAssembler()
		var got []au
		var keyLast time.Time
		for _, x := range arr {
			if x.p.Timestamp == 10*3000 {
				keyLast = x.at
			}
			got = append(got, a.push(x.p, x.at)...)
		}
		var key *au
		for i := range got {
			if got[i].ts == 10*3000 {
				key = &got[i]
			}
		}
		if key == nil || !key.key || !same(key.data, aus[10]) || !key.gap {
			t.Fatalf("%s: the keyframe didn't come out whole after the gap", c.name)
		}
		if d := key.at.Sub(keyLast); d != 0 {
			t.Fatalf("%s: the keyframe came out %v after its last packet", c.name, d)
		}
		if want := plan[first[2]].at.Add(lossWait); key.lost.Before(want) || key.lost.After(want.Add(40*time.Millisecond)) {
			t.Fatalf("%s: the loss found at %v, want about %v", c.name, key.lost.Sub(start), want.Sub(start))
		}
		// Unit 0, then 10 to 19.
		if len(got) != 11 || a.Skipped != c.skip || a.awaitingKey() {
			t.Fatalf("%s: %d units out, %d skipped, awaiting a keyframe %v", c.name, len(got), a.Skipped, a.awaitingKey())
		}
	}
}

// TestAssembleSkipsFillerTail: while a keyframe is awaited, a probe's
// filler (an SEI after the slice, in packets of its own) that comes a push
// after its frame's slice was dropped goes too, not out as a unit.
func TestAssembleSkipsFillerTail(t *testing.T) {
	aus := units(12, 10)
	aus[3] = appendFiller(aus[3], 3000) // three packets of filler after the slice
	pkts := packetize(aus)
	a := newAssembler()
	now := time.Unix(1000, 0)
	var got []au
	for i, p := range pkts {
		if p.Timestamp == pkts[0].Timestamp+3*3000 && a.next == p.SequenceNumber && !a.awaitingKey() {
			a.awaitKey(now) // the decoder needs a keyframe from unit 3 on
		}
		got = append(got, a.push(p, now.Add(time.Duration(i)*time.Millisecond))...)
	}
	// Units 0 to 2, then the keyframe at 10 and 11: 3 to 9 skipped.
	if len(got) != 5 || !got[3].key || a.Skipped != 7 {
		t.Fatalf("%d units out (want 5), the fourth a keyframe %v; %d skipped (want 7)", len(got), len(got) > 3 && got[3].key, a.Skipped)
	}
}

// withPadding puts n padding-only packets after each unit's marker, as the
// SFU pads a probe between frames: sequence numbers of their own in the
// one unbroken run, the timestamp of the frame before them, no payload.
func withPadding(pkts []*rtp.Packet, n int) []*rtp.Packet {
	var out []*rtp.Packet
	seq := pkts[0].SequenceNumber
	for _, p := range pkts {
		q := *p
		q.SequenceNumber = seq
		seq++
		out = append(out, &q)
		if !p.Marker {
			continue
		}
		for range n {
			out = append(out, &rtp.Packet{Header: rtp.Header{Version: 2, Padding: true, PaddingSize: 255, SequenceNumber: seq, Timestamp: p.Timestamp, PayloadType: p.PayloadType, SSRC: p.SSRC}})
			seq++
		}
	}
	return out
}

// TestAssembleProbePadding: the SFU's probe pads between frames with
// packets of padding alone, numbered in the stream and stamped with the
// frame before them. They're neither a hole nor a unit, and no unit ends
// at one: in order; with one resent late (after the next frame began);
// while a frame's own packet is waited for; and while a keyframe is
// awaited, where they go with the frame skipped before them.
func TestAssembleProbePadding(t *testing.T) {
	aus := units(30, 10)
	start := time.Unix(1000, 0)
	// push sends the packets 1 ms apart in the order given, and returns the
	// units out.
	push := func(a *assembler, pkts []*rtp.Packet) []au {
		var got []au
		for i, p := range pkts {
			got = append(got, a.push(p, start.Add(time.Duration(i)*time.Millisecond))...)
		}
		return got
	}
	check := func(name string, a *assembler, got []au, want []int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d units out, want %d (dropped %d, cut %d, skipped %d)", name, len(got), len(want), a.Dropped, a.Cut, a.Skipped)
		}
		for i, u := range got {
			if !same(u.data, aus[want[i]]) {
				t.Fatalf("%s: unit %d out isn't unit %d", name, i, want[i])
			}
		}
	}
	all := make([]int, len(aus))
	for i := range all {
		all[i] = i
	}
	padded := withPadding(packetize(aus), 4)

	// In order: every unit, none with a gap.
	a := newAssembler()
	got := push(a, padded)
	check("in order", a, got, all)
	for i, u := range got {
		if u.gap {
			t.Fatalf("in order: unit %d says a unit was lost", i)
		}
	}

	// One padding packet comes late, after the next frame's packets: it
	// fills its hole when it comes, and nothing is given up for it.
	var late []*rtp.Packet
	var held *rtp.Packet
	sent := false
	for i, p := range padded {
		if held == nil && len(p.Payload) == 0 && i > 40 && len(padded[i-1].Payload) == 0 {
			held = p // the second padding packet of a gap
			continue
		}
		late = append(late, p)
		if held != nil && !sent && p.Marker && p.Timestamp != held.Timestamp {
			late = append(late, held) // a frame later
			sent = true
		}
	}
	if !sent {
		t.Fatal("padding late: nothing held back")
	}
	a = newAssembler()
	check("padding late", a, push(a, late), all)
	if a.Dropped != 0 || a.gap {
		t.Fatalf("padding late: %d packets given up", a.Dropped)
	}

	// A frame's packet is waited for while padding and the next frames
	// arrive; it comes 100 ms later, and everything after it comes out
	// then, the padding with nothing.
	var lost int
	for i, p := range padded {
		if p.Timestamp == padded[0].Timestamp+12*3000 && len(p.Payload) > 0 && !p.Marker {
			lost = i // unit 12's first packet
			break
		}
	}
	a = newAssembler()
	var out []au
	for i, p := range padded {
		at := start.Add(time.Duration(i) * time.Millisecond)
		if i == lost {
			continue
		}
		out = append(out, a.push(p, at)...)
		if at.Sub(start.Add(time.Duration(lost)*time.Millisecond)) >= 100*time.Millisecond && lost >= 0 {
			out = append(out, a.push(padded[lost], at)...)
			lost = -1
		}
	}
	check("a hole mended", a, out, all)

	// The same packet never comes: unit 12 is given up, the stream picks
	// up at the next keyframe (20), and the padding between is skipped
	// with the frames before it.
	for i, p := range padded {
		if p.Timestamp == padded[0].Timestamp+12*3000 && len(p.Payload) > 0 && !p.Marker {
			lost = i
			break
		}
	}
	a = newAssembler()
	out = nil
	for i, p := range padded {
		if i != lost {
			out = append(out, a.push(p, start.Add(time.Duration(i)*10*time.Millisecond))...)
		}
	}
	want := append(slices.Clone(all[:12]), all[20:]...)
	check("a hole given up", a, out, want)
	if !out[12].gap || !out[12].key || a.Skipped == 0 {
		t.Fatalf("a hole given up: after it %v (gap %v), %d skipped", out[12].ts, out[12].gap, a.Skipped)
	}
}

// TestAssembleFillerStandIn: the SFU sends a publisher's probe filler (an
// SEI after a frame's slices, in packets of its own, the last with the
// frame's marker) as padding-only packets that keep their sequence numbers
// and the marker: the frame ends at its stand-in's marker, and nothing of
// the stand-ins goes into it.
func TestAssembleFillerStandIn(t *testing.T) {
	aus := units(12, 10)
	filled := make([][]byte, len(aus))
	for i, a := range aus {
		filled[i] = appendFiller(append([]byte(nil), a...), 2500)
	}
	var pkts []*rtp.Packet
	stood, marked := 0, 0
	for _, p := range packetize(filled) {
		if carries(p.Payload) == carriesOther && len(p.Payload) > 0 && (p.Payload[0]&0x1f == 6 || p.Payload[0]&0x1f == nalFUA) {
			p = &rtp.Packet{Header: rtp.Header{Version: 2, Padding: true, Marker: p.Marker, SequenceNumber: p.SequenceNumber, Timestamp: p.Timestamp}}
			stood++
			if p.Marker {
				marked++
			}
		}
		pkts = append(pkts, p)
	}
	if stood < 2*len(aus) || marked != len(aus) {
		t.Fatalf("%d stand-ins, %d with a marker", stood, marked)
	}
	a := newAssembler()
	var got []au
	for i, p := range pkts {
		got = append(got, a.push(p, time.Unix(1000, 0).Add(time.Duration(i)*time.Millisecond))...)
	}
	if len(got) != len(aus) {
		t.Fatalf("%d units out, want %d (dropped %d, cut %d)", len(got), len(aus), a.Dropped, a.Cut)
	}
	for i, u := range got {
		if !same(u.data, aus[i]) {
			t.Fatalf("unit %d out isn't unit %d without its filler", i, i)
		}
	}
}

func TestKeyframeStart(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"an SPS", []byte{0x67, 0x42, 0xe0, 0x1f}, true},
		{"a STAP-A of SPS and PPS", []byte{0x78, 0, 2, 0x67, 0x42, 0, 1, 0x68}, true},
		{"a STAP-A of a PPS", []byte{0x78, 0, 1, 0x68}, false},
		{"a STAP-A cut short", []byte{0x78, 0, 9, 0x67}, false},
		{"an SPS's first fragment", []byte{0x7c, 0x87, 1}, true},
		{"an SPS's second fragment", []byte{0x7c, 0x07, 1}, false},
		{"an IDR slice's first fragment", []byte{0x7c, 0x85, 1}, false},
		{"a P slice", []byte{0x41, 0x9a}, false},
		{"padding", nil, false},
	} {
		if got := keyframeStart(c.payload); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// What our packetizing makes of a keyframe: the first packet carries
	// the SPS, the IDR's fragments don't.
	pkts := packetize(units(1, 1))
	if !keyframeStart(pkts[0].Payload) || keyframeStart(pkts[1].Payload) {
		t.Fatal("a packetized keyframe's first packets read wrong")
	}
}
