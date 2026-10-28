package room

import (
	"testing"
	"time"
)

// TestAssembleCut: a unit cut off before its marker (the SFU switched the
// viewer to another layer) is dropped, not glued onto what follows: at a
// keyframe's first packet, whether or not it has the cut unit's timestamp
// (a publication's layers share one clock), or at a packet with another
// timestamp, after which the decoder is told a unit was lost.
func TestAssembleCut(t *testing.T) {
	aus := units(10, 10) // a keyframe, then P slices
	other := units(4, 4) // another layer from its keyframe
	for _, c := range []struct {
		name   string
		next   [][]byte // what follows the cut, sent on unbroken
		sameTS bool
		gap    bool // the unit after the cut says one was lost
	}{
		{"a keyframe at the same time", other, true, false},
		{"a later keyframe", other, false, false},
		{"a later P slice", aus[6:], false, true},
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
		want := append(append([][]byte(nil), aus[:5]...), c.next...)
		if len(got) != len(want) || a.Cut != 1 {
			t.Fatalf("%s: %d units out, want %d; %d cut", c.name, len(got), len(want), a.Cut)
		}
		for i, u := range got {
			if !same(u.data, want[i]) {
				t.Fatalf("%s: unit %d out differs", c.name, i)
			}
			if u.gap != (i == 5 && c.gap) {
				t.Fatalf("%s: unit %d out: gap %v", c.name, i, u.gap)
			}
		}
		if got[5].key == c.gap {
			t.Fatalf("%s: the unit after the cut: key %v", c.name, got[5].key)
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
