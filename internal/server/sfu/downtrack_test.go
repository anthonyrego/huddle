package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// A down track's sequence numbers run on unbroken: through a reordered
// packet, a packet of padding alone (not sent), and the publisher's stream
// starting again under a new SSRC (sent the track again), whose timestamps
// carry on from the old stream's by the time between them.
func TestMunge(t *testing.T) {
	d := &downTrack{clock: 48000}
	at := time.Unix(1000, 0)
	pkt := func(ssrc uint32, seq uint16, ts uint32, payload string) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SSRC: ssrc, SequenceNumber: seq, Timestamp: ts}, Payload: []byte(payload)}
	}
	type out struct {
		seq uint16
		ts  uint32
		ok  bool
	}
	send := func(p *rtp.Packet, now time.Time) out {
		seq, ts, ok := d.munge(p, now)
		return out{seq, ts, ok}
	}

	first := send(pkt(1, 65534, 1000, "x"), at)
	if !first.ok || first.ts != 1000 {
		t.Fatalf("first packet: %+v, want sent with its timestamp", first)
	}
	base := first.seq
	steps := []struct {
		p    *rtp.Packet
		now  time.Duration
		want out
	}{
		{pkt(1, 65535, 1960, "x"), 20 * time.Millisecond, out{base + 1, 1960, true}},
		{pkt(1, 1, 3880, "x"), 60 * time.Millisecond, out{base + 3, 3880, true}}, // 0 comes late
		{pkt(1, 0, 2920, "x"), 61 * time.Millisecond, out{base + 2, 2920, true}},
		{pkt(1, 2, 4840, ""), 80 * time.Millisecond, out{}}, // padding alone: not sent
		{pkt(1, 3, 5800, "x"), 100 * time.Millisecond, out{base + 4, 5800, true}},
		// A new stream 500 ms later, numbered and timed afresh.
		{pkt(2, 40000, 900000, "x"), 600 * time.Millisecond, out{base + 5, 5800 + 24000, true}},
		{pkt(2, 40001, 900960, "x"), 620 * time.Millisecond, out{base + 6, 5800 + 24960, true}},
		// And straight back to the first: a frame's gap at least.
		{pkt(1, 4, 6760, "x"), 621 * time.Millisecond, out{base + 7, 5800 + 24960 + 960, true}},
	}
	for i, s := range steps {
		if got := send(s.p, at.Add(s.now)); got != s.want {
			t.Errorf("step %d (ssrc %d seq %d): %+v, want %+v", i, s.p.SSRC, s.p.SequenceNumber, got, s.want)
		}
	}
}

func TestToNTP(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 250_000_000, time.UTC)
	got := toNTP(at)
	if sec := got >> 32; sec != uint64(at.Unix())+ntpEpoch {
		t.Errorf("seconds %d", sec)
	}
	if frac := got & 0xffffffff; frac != 1<<30 {
		t.Errorf("fraction %#x, want a quarter", frac)
	}
}
