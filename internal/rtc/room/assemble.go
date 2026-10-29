package room

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
// shows); lossWait after it showed (a later packet came), not after the
// assembler got to it, the units it belonged to are given up, and the next
// unit out says a gap came before it, so the decoder waits for a keyframe.
// (Timed from when the assembler got to them, holes never mended queued
// one wait behind another, and under heavy loss the video fell seconds
// behind.)
//
// While the decoder waits for a keyframe (awaitKey: after a hole given up,
// or when the decoder asks), a unit that isn't one is dropped as soon as a
// packet shows it isn't, holes and all: nothing waits for a frame that
// would be thrown away, and the keyframe goes out as soon as it's whole.
//
// A unit also ends where the next one starts without its marker: at a
// packet with another timestamp, or at a keyframe's first packet. That's a
// frame cut off, as the SFU does when it switches a viewer to another
// layer at that layer's keyframe (which may share the cut frame's
// timestamp: a publication's layers share one clock). The cut unit is
// dropped rather than glued onto what follows. A keyframe's first packet
// is also where the assembler starts, and where it picks up after a hole
// it gave up on, so the keyframe that follows a lost packet isn't lost
// with it.
type assembler struct {
	pkts map[uint16]*heldPkt
	// missing are the packets not here that a later one came after, and
	// when the first of those came: when the hole showed.
	missing map[uint16]time.Time
	next    uint16 // the first packet of the next unit
	newest  uint16
	synced  bool      // next is the start of a unit
	gap     bool      // units were given up since the last one out
	needKey bool      // the decoder waits for a keyframe
	lostAt  time.Time // when the loss it waits for one after was found
	// The timestamp of the last frame skipToKey dropped (if skipping).
	skipping  bool
	skippedTS uint32
	Dropped   int // packets given up
	Cut       int // units dropped because the next began before their end
	Skipped   int // units dropped while waiting for a keyframe
}

// heldPkt is a packet waiting to go out in a unit, and when it came.
type heldPkt struct {
	*rtp.Packet
	at time.Time
}

// au is one access unit.
type au struct {
	data []byte
	ts   uint32 // RTP timestamp, 90 kHz
	key  bool
	gap  bool      // units were lost before it
	lost time.Time // with gap: when the loss was found
	last time.Time // when the last of its packets came
	at   time.Time // when it was put together (it may have waited for a hole before it)
}

// lossWait is how long a missing packet is waited for.
const lossWait = 250 * time.Millisecond

// maxHeld bounds the packets held while waiting.
const maxHeld = 4000

func newAssembler() *assembler {
	return &assembler{pkts: map[uint16]*heldPkt{}, missing: map[uint16]time.Time{}}
}

// awaitKey says the decoder needs a keyframe (the loss found at now): what
// isn't one is dropped until one comes.
func (a *assembler) awaitKey(now time.Time) {
	if !a.needKey {
		a.needKey, a.lostAt = true, now
	}
}

// awaitingKey reports whether a keyframe is awaited (since lostAt).
func (a *assembler) awaitingKey() bool { return a.needKey }

// after reports whether sequence number a comes after b.
func after(a, b uint16) bool { return int16(a-b) > 0 }

// push takes one packet and returns the units it completes.
func (a *assembler) push(p *rtp.Packet, now time.Time) []au {
	seq := p.SequenceNumber
	if !a.synced {
		switch {
		case keyframeStart(p.Payload):
			a.next = seq
		case p.Marker:
			// Start after the first marker: the unit before it may have
			// begun before we joined.
			a.next = seq + 1
		default:
			a.pkts[seq] = &heldPkt{p, now}
			return nil
		}
		a.newest, a.synced = seq, true
		for s := range a.pkts {
			switch {
			case !after(s, a.next-1):
				delete(a.pkts, s)
			case after(s, a.newest):
				a.newest = s
			}
		}
		if a.next == seq {
			a.pkts[seq] = &heldPkt{p, now}
		}
		return a.drain(now)
	}
	if !after(seq, a.next-1) {
		return nil // already out or given up: a late copy
	}
	if _, dup := a.pkts[seq]; dup {
		return nil
	}
	a.pkts[seq] = &heldPkt{p, now}
	delete(a.missing, seq)
	if after(seq, a.newest) {
		// The packets jumped over are holes from now (the newest maxHeld
		// of them; one further back is timed from when it's reached).
		from := a.newest + 1
		if seq-from > maxHeld {
			from = seq - maxHeld
		}
		for s := from; s != seq; s++ {
			a.missing[s] = now
		}
		a.newest = seq
	}
	return a.drain(now)
}

// release lets go of the packets from through to (not included), and
// returns how many were here.
func (a *assembler) release(from, to uint16) int {
	n := 0
	for s := from; s != to; s++ {
		if _, ok := a.pkts[s]; ok {
			delete(a.pkts, s)
			n++
		}
		delete(a.missing, s)
	}
	return n
}

// drain sends out what is complete from next on, dropping a unit cut off
// by the next and giving up a hole that has waited too long.
func (a *assembler) drain(now time.Time) []au {
	var out []au
	for {
		if a.needKey {
			a.skipToKey()
		}
		// Padding between units (the SFU probing the link) carries nothing.
		for p := a.pkts[a.next]; p != nil && len(p.Payload) == 0 && !p.Marker; p = a.pkts[a.next] {
			delete(a.pkts, a.next)
			a.next++
		}
		end, complete, cut := a.next, false, false
		var ts uint32
		begun := false // a packet with a payload has been seen: ts is the unit's
		for ; ; end++ {
			p := a.pkts[end]
			if p == nil {
				break
			}
			if len(p.Payload) > 0 {
				if begun && (p.Timestamp != ts || keyframeStart(p.Payload)) {
					cut = true
					break
				}
				ts, begun = p.Timestamp, true
			}
			if p.Marker {
				complete = true
				break
			}
			if end-a.next > maxHeld {
				break
			}
		}
		if cut {
			// The next unit starts at end: what came before it is a frame
			// cut off. After a keyframe nothing is missing; otherwise the
			// decoder may need the frame dropped.
			a.release(a.next, end)
			a.Cut++
			if !keyframeStart(a.pkts[end].Payload) {
				a.gap = true
				a.awaitKey(now)
			}
			a.next = end
			continue
		}
		if complete {
			u := a.unit(a.next, end, now)
			a.next = end + 1
			if u.data != nil {
				out = append(out, u)
			}
			continue
		}
		// Nothing missing yet: the unit is still arriving.
		if a.pkts[end] == nil && !after(a.newest, end) {
			return out
		}
		// A hole at end, with later packets here: waited for lossWait
		// from when it showed.
		seen, ok := a.missing[end]
		if !ok {
			seen = now
			a.missing[end] = now
		}
		if now.Sub(seen) < lossWait && len(a.pkts) < maxHeld {
			return out
		}
		// Give up through the hole, to where the next whole unit starts.
		resume, ok := a.nextStart(end)
		if !ok {
			if len(a.pkts) < maxHeld {
				return out
			}
			resume = a.newest + 1
		}
		a.Dropped += a.release(a.next, resume)
		a.next, a.gap = resume, true
		a.awaitKey(now)
	}
}

// What an H.264 RTP payload carries, for skipToKey.
const (
	carriesOther    = iota // padding, SEI: nothing that says which frame
	carriesKeyStart        // a keyframe's first packet (its SPS)
	carriesKey             // more of a keyframe: its PPS or IDR slice
	carriesNonKey          // a slice of a frame that isn't a keyframe
)

func carries(payload []byte) int {
	if keyframeStart(payload) {
		return carriesKeyStart
	}
	if len(payload) == 0 {
		return carriesOther
	}
	kind := func(t byte) int {
		switch t {
		case nalIDR, nalSPS, nalPPS:
			return carriesKey
		case 1, 2, 3, 4: // a non-IDR slice, or one of its partitions
			return carriesNonKey
		}
		return carriesOther
	}
	switch t := payload[0] & 0x1f; t {
	case nalFUA:
		if len(payload) < 2 {
			return carriesOther
		}
		return kind(payload[1] & 0x1f)
	case nalSTAPA:
		k := carriesOther
		for i := 1; i+2 < len(payload); {
			n := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if n == 0 || i+n > len(payload) {
				break
			}
			if c := kind(payload[i] & 0x1f); c != carriesOther {
				k = c
			}
			i += n
		}
		return k
	default:
		return kind(t)
	}
}

// skipToKey drops, while a keyframe is awaited, every packet from next on
// that's known not to be one's: a slice of another frame, padding or SEI
// after one, and the holes among them (a frame thrown away needs nothing
// mended). It stops at a keyframe's first packet, and at a packet of a
// keyframe whose first packet isn't here (it may be in the hole before:
// that hole is waited for as usual, and given up, it's skipped past).
func (a *assembler) skipToKey() {
	resume := a.next // what's before it goes
	// The last frame known not to be a keyframe (its tail, an SEI filler
	// that may come a push later, goes with it).
	nonKey, nonKeyTS := a.skipping, a.skippedTS
	found := false
scan:
	for s := a.next; !after(s, a.newest); s++ {
		p := a.pkts[s]
		if p == nil {
			continue
		}
		switch carries(p.Payload) {
		case carriesKeyStart:
			resume, found = s, true
			break scan
		case carriesKey:
			found = true
			break scan
		case carriesNonKey:
			resume, nonKey, nonKeyTS = s+1, true, p.Timestamp
		default:
			if nonKey && p.Timestamp == nonKeyTS {
				resume = s + 1 // the tail of that frame
			}
		}
	}
	if resume != a.next {
		for s := a.next; s != resume; s++ {
			// A frame is counted once, though its packets may go a few at a
			// time as they come.
			if p := a.pkts[s]; p != nil && len(p.Payload) > 0 && (!a.skipping || p.Timestamp != a.skippedTS) {
				a.Skipped++
				a.skipping, a.skippedTS = true, p.Timestamp
			}
		}
		a.release(a.next, resume)
		a.next = resume
	}
	if found {
		a.needKey = false
	}
}

// nextStart is where the next whole unit starts at or past from: a
// keyframe's first packet, or the packet after a marker, whichever comes
// first.
func (a *assembler) nextStart(from uint16) (uint16, bool) {
	for s := from; !after(s, a.newest); s++ {
		p := a.pkts[s]
		switch {
		case p == nil:
		case keyframeStart(p.Payload):
			return s, true
		case p.Marker:
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
	if u.gap {
		u.lost = a.lostAt
	}
	for s := first; ; s++ {
		p := a.pkts[s]
		delete(a.pkts, s)
		if p.at.After(u.last) {
			u.last = p.at
		}
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

// H.264 NAL unit types the assembler looks for in RTP payloads (RFC 6184).
const (
	nalIDR   = 5
	nalSPS   = 7
	nalPPS   = 8
	nalSTAPA = 24
	nalFUA   = 28
)

// keyframeStart says whether an H.264 RTP payload is the first packet of a
// keyframe: it carries the SPS, which our encoder puts first in every
// keyframe (macvideo.Encoder: SPS and PPS ahead of each IDR). The payload
// is a single NAL unit, a STAP-A (pion's payloader puts the SPS and PPS in
// one), or the first fragment of an FU-A. (The SFU has its own copy, in
// internal/server/sfu's h264.go.)
func keyframeStart(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	switch t := payload[0] & 0x1f; {
	case t == nalSTAPA:
		for i := 1; i+2 < len(payload); {
			n := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if n == 0 || i+n > len(payload) {
				return false
			}
			if payload[i]&0x1f == nalSPS {
				return true
			}
			i += n
		}
		return false
	case t == nalFUA:
		return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&0x1f == nalSPS
	default:
		return t == nalSPS
	}
}
