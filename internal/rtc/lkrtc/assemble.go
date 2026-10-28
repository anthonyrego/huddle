package lkrtc

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/anthonyrego/huddle/internal/video"
)

// assembler puts a video track's H.264 RTP packets back together into
// access units (Annex-B), in order. A unit is complete when every packet
// from the one after the last unit's marker to the next marker is here;
// it goes out at once (pion's samplebuilder waits for the next unit's first
// packet, a frame's time later, and on a still screen share for as long as
// nothing changes). A missing packet holds what follows while a
// retransmission may still come (the NACKs go out as soon as the gap
// shows); after lossWait the units it belonged to are given up, and the
// next unit out says a gap came before it, so the decoder waits for a
// keyframe.
type assembler struct {
	pkts    map[uint16]*rtp.Packet
	next    uint16 // the first packet of the next unit
	newest  uint16
	synced  bool      // next is the start of a unit
	holeAt  time.Time // when the hole at the front was first seen
	gap     bool      // units were given up since the last one out
	Dropped int       // packets given up
}

// au is one access unit.
type au struct {
	data []byte
	ts   uint32 // RTP timestamp, 90 kHz
	key  bool
	gap  bool      // units were lost before it
	at   time.Time // when its last packet came
}

// lossWait is how long a missing packet is waited for.
const lossWait = 250 * time.Millisecond

// maxHeld bounds the packets held while waiting.
const maxHeld = 4000

func newAssembler() *assembler { return &assembler{pkts: map[uint16]*rtp.Packet{}} }

// after reports whether sequence number a comes after b.
func after(a, b uint16) bool { return int16(a-b) > 0 }

// push takes one packet and returns the units it completes.
func (a *assembler) push(p *rtp.Packet, now time.Time) []au {
	seq := p.SequenceNumber
	if !a.synced {
		// Start after the first marker: the unit before it may have begun
		// before we joined.
		if p.Marker {
			a.next, a.newest, a.synced = seq+1, seq, true
			for s := range a.pkts {
				if !after(s, seq) {
					delete(a.pkts, s)
				}
			}
		} else {
			a.pkts[seq] = p
		}
		return nil
	}
	if !after(seq, a.next-1) {
		return nil // already out or given up: a late copy
	}
	if _, dup := a.pkts[seq]; dup {
		return nil
	}
	a.pkts[seq] = p
	if after(seq, a.newest) {
		a.newest = seq
	}
	return a.drain(now)
}

// drain sends out what is complete from next on, giving up a hole that
// has waited too long.
func (a *assembler) drain(now time.Time) []au {
	var out []au
	for {
		// Padding between units (the SFU probing the link) carries nothing.
		for p := a.pkts[a.next]; p != nil && len(p.Payload) == 0 && !p.Marker; p = a.pkts[a.next] {
			delete(a.pkts, a.next)
			a.next++
		}
		end, complete := a.next, false
		for ; ; end++ {
			p := a.pkts[end]
			if p == nil {
				break
			}
			if p.Marker {
				complete = true
				break
			}
			if end-a.next > maxHeld {
				break
			}
		}
		if complete {
			u := a.unit(a.next, end, now)
			a.next = end + 1
			a.holeAt = time.Time{}
			if u.data != nil {
				out = append(out, u)
			}
			continue
		}
		// Nothing missing yet: the unit is still arriving.
		if a.pkts[end] == nil && !after(a.newest, end) {
			return out
		}
		// A hole at end, with later packets here.
		if a.holeAt.IsZero() {
			a.holeAt = now
		}
		if now.Sub(a.holeAt) < lossWait && len(a.pkts) < maxHeld {
			return out
		}
		// Give up through the first unit that ends after the hole.
		resume, ok := a.nextStart(end)
		if !ok {
			if len(a.pkts) < maxHeld {
				return out
			}
			resume = a.newest + 1
		}
		for s := a.next; s != resume; s++ {
			if _, ok := a.pkts[s]; ok {
				delete(a.pkts, s)
				a.Dropped++
			}
		}
		a.next, a.gap, a.holeAt = resume, true, time.Time{}
	}
}

// nextStart is the first packet after a marker at or past from: where the
// next whole unit starts.
func (a *assembler) nextStart(from uint16) (uint16, bool) {
	for s := from; !after(s, a.newest); s++ {
		if p := a.pkts[s]; p != nil && p.Marker {
			return s + 1, true
		}
	}
	return 0, false
}

// unit depacketizes packets first through last (inclusive) into one
// access unit and lets them go.
func (a *assembler) unit(first, last uint16, now time.Time) au {
	var dp codecs.H264Packet
	u := au{gap: a.gap, at: now}
	for s := first; ; s++ {
		p := a.pkts[s]
		delete(a.pkts, s)
		if len(p.Payload) > 0 {
			u.ts = p.Timestamp
			if b, err := dp.Unmarshal(p.Payload); err == nil {
				u.data = append(u.data, b...)
			}
		}
		if s == last {
			break
		}
	}
	if u.data != nil {
		a.gap = false
		u.key = video.IsKeyframe(u.data)
	}
	return u
}
