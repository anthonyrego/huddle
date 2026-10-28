package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// A synthetic simulcast camera: layer 0 sends 6 packets a frame, layer 1
// three, layer 2 one; each layer keyframes at its first frame, every 60
// frames after (at a phase of its own), and on the frame after a keyframe
// is asked for. Every payload ends in a tag naming its layer, frame and
// place in the frame.
var synthPackets = []int{6, 3, 1}

type synth struct {
	seq   [3]uint16
	askAt [3]bool
	keys  [3]int
}

const tagLen = 5

func (s *synth) frame(layer, n int) []rtp.Packet {
	k := synthPackets[layer]
	key := n == 0 || (n+20*layer)%60 == 0 || s.askAt[layer]
	if key {
		s.askAt[layer] = false
		s.keys[layer]++
	}
	var out []rtp.Packet
	for i := range k {
		var p []byte
		switch {
		case i == 0 && key:
			p = []byte{nalSTAPA, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xce}
		case i == 0:
			p = []byte{0x7c, 0x81}
		case key:
			p = []byte{0x7c, 0x05}
		default:
			p = []byte{0x7c, 0x01}
		}
		p = append(p, byte(layer), byte(n>>8), byte(n), byte(i), byte(k))
		s.seq[layer]++
		out = append(out, rtp.Packet{
			Header:  rtp.Header{SequenceNumber: s.seq[layer] + uint16(layer)*10000, Timestamp: uint32(n) * 3000, Marker: i == k-1},
			Payload: p,
		})
	}
	return out
}

type tag struct{ layer, frame, idx, n int }

func tagOf(p []byte) tag {
	t := p[len(p)-tagLen:]
	return tag{int(t[0]), int(t[1])<<8 | int(t[2]), int(t[3]), int(t[4])}
}

type sent struct {
	seq uint16
	tag tag
	key bool
}

// checkStream fails unless what the viewer got is one unbroken stream of
// whole frames, each layer change starting at a keyframe, frame numbers
// going back only at a layer change (the new layer lagging the old).
func checkStream(t *testing.T, out []sent) {
	t.Helper()
	for i := 1; i < len(out); i++ {
		if out[i].seq != out[i-1].seq+1 {
			t.Fatalf("packet %d: sequence %d after %d", i, out[i].seq, out[i-1].seq)
		}
	}
	lastFrame, lastLayer := -1, -1
	for i := 0; i < len(out); {
		first := out[i].tag
		if first.idx != 0 {
			t.Fatalf("packet %d: a frame starting at packet %d of %v", i, first.idx, first)
		}
		for j := 1; j < first.n; j++ {
			if i+j >= len(out) {
				return // the run ended mid-frame
			}
			if g := out[i+j].tag; g.layer != first.layer || g.frame != first.frame || g.idx != j {
				t.Fatalf("packet %d: %v inside frame %v", i+j, g, first)
			}
		}
		if first.layer != lastLayer && !out[i].key {
			t.Fatalf("packet %d: layer %d → %d at frame %d, not a keyframe", i, lastLayer, first.layer, first.frame)
		}
		if first.frame < lastFrame && first.layer == lastLayer {
			t.Fatalf("packet %d: frame %d after frame %d", i, first.frame, lastFrame)
		}
		lastFrame, lastLayer = first.frame, first.layer
		i += first.n
	}
}

// run plays frames 0 to frames-1 of a synthetic camera through a forwarder,
// changing the target at the frames in targets (layer, or -1 for none).
// lagLayer's frames arrive after the other layers' next frame. hold, when
// set, takes each packet and says whether to hold it back to the end of
// frame+3 (a retransmission).
func run(t *testing.T, frames int, targets map[int]int, lagLayer int, hold func(tag) bool) (out []sent, switched map[int]int, f *forwarder, s *synth) {
	s = &synth{}
	f = newForwarder(40000)
	emit := func(seq uint16, h *rtp.Header, payload []byte) {
		out = append(out, sent{seq, tagOf(payload), keyframeStart(payload)})
	}
	t0 := time.Unix(0, 0)
	switched = map[int]int{}
	lastCurrent := -1
	var lagged []rtp.Packet
	late := map[int][]rtp.Packet{}
	deliver := func(p rtp.Packet, now time.Time) {
		layer := tagOf(p.Payload).layer
		if f.push(layer, &p.Header, p.Payload, now, emit) {
			s.askAt[layer] = true
		}
	}
	for n := range frames {
		now := t0.Add(time.Duration(n) * 33 * time.Millisecond)
		if l, ok := targets[n]; ok {
			if f.setTarget(l) && l >= 0 {
				s.askAt[l] = true
			}
		}
		var pkts []rtp.Packet
		for layer := 2; layer >= 0; layer-- {
			for _, p := range s.frame(layer, n) {
				if hold != nil && hold(tagOf(p.Payload)) {
					late[n+3] = append(late[n+3], p)
					continue
				}
				if layer == lagLayer {
					lagged = append(lagged, p)
					continue
				}
				pkts = append(pkts, p)
			}
		}
		for _, p := range pkts {
			deliver(p, now)
		}
		// The lagging layer's previous frame comes after this one's others.
		var keep []rtp.Packet
		for _, p := range lagged {
			if tagOf(p.Payload).frame < n {
				deliver(p, now)
			} else {
				keep = append(keep, p)
			}
		}
		lagged = keep
		for _, p := range late[n] {
			deliver(p, now)
		}
		if f.current != lastCurrent {
			switched[n] = f.current
			lastCurrent = f.current
		}
	}
	return out, switched, f, s
}

func TestForwarderSwitches(t *testing.T) {
	targets := map[int]int{0: 2, 30: 0, 75: 1, 100: -1, 120: 0, 150: 2}
	out, switched, f, s := run(t, 200, targets, -1, nil)
	checkStream(t, out)
	t.Logf("switched (frame: layer) %v; keyframes per layer %v; %d packets", switched, s.keys, len(out))
	if f.backs != 0 {
		t.Errorf("%d switches stepped back in time", f.backs)
	}
	// Each switch lands within two frames of being asked for: the
	// keyframe asked for comes on the next frame.
	for at, layer := range targets {
		ok := false
		for n := at; n <= at+2; n++ {
			if l, hit := switched[n]; hit && l == layer {
				ok = true
			}
		}
		if !ok {
			t.Errorf("target %d at frame %d: switches %v", layer, at, switched)
		}
	}
	// Nothing is sent while the track is off.
	for _, p := range out {
		if p.tag.frame > 100 && p.tag.frame < 120 {
			t.Fatalf("frame %d sent while off", p.tag.frame)
		}
	}
}

func TestForwarderLateKeyframe(t *testing.T) {
	// Layer 0's frames arrive after the others' next frame, so the
	// keyframe asked for at frame 30 is older than what layer 2 has sent:
	// the switch goes ahead anyway, one frame back.
	targets := map[int]int{0: 2, 30: 0}
	out, switched, f, s := run(t, 90, targets, 0, nil)
	checkStream(t, out)
	t.Logf("switched %v; keyframes %v; stepped back %d", switched, s.keys, f.backs)
	if switched[31] != 0 || f.backs != 1 {
		t.Errorf("switches %v, stepped back %d: want layer 0 at frame 31, once back", switched, f.backs)
	}
}

func TestForwarderLateRetransmission(t *testing.T) {
	// Packet 3 of layer 0's frame 29 is lost, and its retransmission comes
	// three frames later: after the switch to layer 2 at frame 30. It goes
	// out in the gap it left.
	targets := map[int]int{0: 0, 30: 2}
	hold := func(g tag) bool { return g.layer == 0 && g.frame == 29 && g.idx == 3 }
	out, _, _, _ := run(t, 60, targets, -1, hold)
	var gap, filled bool
	seen := map[uint16]tag{}
	min, max := out[0].seq, out[0].seq
	for i, p := range out {
		if _, dup := seen[p.seq]; dup {
			t.Fatalf("sequence %d sent twice", p.seq)
		}
		seen[p.seq] = p.tag
		if seqNewer(p.seq, max) {
			max = p.seq
		}
		if seqNewer(min, p.seq) {
			min = p.seq
		}
		if i > 0 && p.seq != out[i-1].seq+1 && seqNewer(p.seq, out[i-1].seq) {
			gap = true
		}
		if p.tag == (tag{0, 29, 3, 6}) {
			filled = true
			if before, ok := seen[p.seq-1]; !ok || before != (tag{0, 29, 2, 6}) {
				t.Errorf("the retransmission went out as %d, after %v", p.seq, before)
			}
		}
	}
	if !gap || !filled {
		t.Fatalf("gap %v, filled %v", gap, filled)
	}
	if n := int(max-min) + 1; n != len(seen) {
		t.Errorf("%d sequence numbers sent of %d between the first and the last", len(seen), n)
	}
}

func TestForwarderSilentLayer(t *testing.T) {
	// Layer 0 stops two packets into frame 30 (its publisher paused it), as
	// layer 2 is asked for: the switch waits silentFor after layer 0's
	// last packet, then cuts its half frame off.
	f := newForwarder(100)
	s := &synth{}
	var out []sent
	emit := func(seq uint16, h *rtp.Header, payload []byte) {
		out = append(out, sent{seq, tagOf(payload), keyframeStart(payload)})
	}
	t0 := time.Unix(0, 0)
	f.setTarget(0)
	at := -1
	for n := 0; n < 40; n++ {
		now := t0.Add(time.Duration(n) * 33 * time.Millisecond)
		if n == 30 {
			f.setTarget(2)
			s.askAt[2] = true
		}
		for layer := 0; layer <= 2; layer++ {
			for i, p := range s.frame(layer, n) {
				if layer == 0 && (n > 30 || n == 30 && i >= 2) {
					continue
				}
				if f.push(layer, &p.Header, p.Payload, now, emit) {
					s.askAt[layer] = true
				}
			}
		}
		if f.current == 2 && at < 0 {
			at = n
		}
	}
	// silentFor (150 ms) after frame 30 is during frame 35.
	if at != 35 {
		t.Errorf("switched at frame %d, want 35", at)
	}
	for i, p := range out {
		if p.tag.layer == 2 {
			if !p.key || p.tag.frame != 30 || out[i-1].tag != (tag{0, 30, 1, 6}) || p.seq != out[i-1].seq+1 {
				t.Errorf("layer 2 began with %+v after %+v", p, out[i-1])
			}
			break
		}
	}
}

func TestKeyframeStart(t *testing.T) {
	cases := []struct {
		name string
		p    []byte
		want bool
	}{
		{"SPS alone", []byte{0x67, 0x42}, true},
		{"IDR alone", []byte{0x65, 0x88}, false},
		{"STAP-A of SPS and PPS", []byte{0x18, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xce}, true},
		{"STAP-A of SEI and SPS", []byte{0x18, 0, 2, 0x06, 0x05, 0, 2, 0x67, 0x42}, true},
		{"STAP-A without SPS", []byte{0x18, 0, 2, 0x06, 0x05, 0, 2, 0x68, 0xce}, false},
		{"STAP-A cut short", []byte{0x18, 0, 9, 0x67}, false},
		{"FU-A start of SPS", []byte{0x7c, 0x87, 0x42}, true},
		{"FU-A start of IDR", []byte{0x7c, 0x85, 0x88}, false},
		{"FU-A middle", []byte{0x7c, 0x07, 0x42}, false},
		{"P slice", []byte{0x41, 0x9a}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := keyframeStart(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
