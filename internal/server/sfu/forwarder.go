package sfu

import (
	"slices"
	"time"

	"github.com/pion/rtp"
)

// Switching layers (forwarder):
const (
	// silentFor is how long the layer being sent may go quiet before a
	// switch stops waiting for it to finish its frame.
	silentFor = 150 * time.Millisecond
	// pendingMost caps the packets held from the new layer's keyframe while
	// the old layer finishes its frame (about a second of 720p).
	pendingMost = 1000
	// pastMost is how many runs before the one being sent are kept for
	// their late packets: a switch ends a run, and so does each probe's
	// padding between frames, 30 a second; the newest 64 cover what a
	// retransmission from the publisher can be late by.
	pastMost = 64
)

// emitFunc sends one packet to the viewer under sequence number seq; the
// header's other fields go as they came.
type emitFunc func(seq uint16, h *rtp.Header, payload []byte)

// forwarder is one viewer's choice of simulcast layer for one video track,
// and the sequence numbers that make what they're sent one unbroken stream
// across layer switches. Timestamps pass through: a publication's layers
// share one timestamp base (the client stamps every layer with the same
// capture clock), so only sequence numbers need rewriting.
//
// The downlink's probe pads between frames (pad): its padding takes the
// sequence numbers after the frame last sent, and the run carries on past
// them, the run before kept for its late packets.
//
// A switch waits for the new layer's keyframe, then for the old layer to
// finish the frame it's in, so the viewer never gets half a frame: the new
// layer's packets from its keyframe on are held meanwhile (unless the old
// layer goes quiet for silentFor; then its half frame is cut off, and the
// client's assembler drops it at the keyframe). When the new layer lags
// the old, its keyframe may be older than the frame last sent, and the
// viewer gets a few frames again at the new size: the client shows only
// frames newer than the last it showed. Waiting for a newer keyframe
// instead would never end if the lag stayed longer than a frame.
//
// Not safe for concurrent use: the down track calls it under its lock (each
// layer arrives on a goroutine of its own).
type forwarder struct {
	target  int // the layer wanted; -1: none (the track is off)
	current int // the layer being sent; -1: none

	lastOut uint16 // the newest sequence number sent
	lastTs  uint32 // the timestamp of the newest packet sent
	open    bool   // the newest packet sent didn't end its frame
	lastAt  time.Time
	cur     segment
	past    []segment // the runs before cur (a switch, padding), newest last, for their late packets

	pending []held // the target's packets from its keyframe on

	backs int // switches whose keyframe was older than the frame last sent
}

// segment is one run of a layer's packets sent: input sequence numbers lo
// to hi, sent as each plus offset.
type segment struct {
	layer  int
	lo, hi uint16
	offset uint16
}

func (s segment) has(layer int, seq uint16) bool {
	return s.layer == layer && !seqNewer(s.lo, seq) && !seqNewer(seq, s.hi)
}

type held struct {
	h       rtp.Header
	payload []byte
}

// newForwarder starts a forwarder whose first packet goes out as start+1.
func newForwarder(start uint16) *forwarder {
	return &forwarder{target: -1, current: -1, lastOut: start, cur: segment{layer: -1}}
}

func seqNewer(a, b uint16) bool { return int16(a-b) > 0 }
func tsNewer(a, b uint32) bool  { return int32(a-b) > 0 }

// setTarget asks for layer (-1 for none). askKey says the layer's keyframe
// is needed (a switch, or a start).
func (f *forwarder) setTarget(layer int) (askKey bool) {
	if layer == f.target {
		return false
	}
	f.target, f.pending = layer, nil
	if layer < 0 {
		if !f.open {
			f.current = -1
		}
		return false
	}
	return layer != f.current
}

// push takes one packet of layer, arriving at now, and sends what the
// viewer gets through emit. askKey says another keyframe is needed: too
// much of the new layer piled up waiting for the old to finish its frame.
func (f *forwarder) push(layer int, h *rtp.Header, payload []byte, now time.Time, emit emitFunc) (askKey bool) {
	switch {
	case layer == f.current && f.current >= 0 && seqNewer(f.cur.lo, h.SequenceNumber):
		// From before this run began: a late packet of an earlier one.
		f.late(layer, h, payload, emit)
	case layer == f.current && f.current >= 0:
		f.send(layer, h, payload, now, emit)
		if f.target < 0 && !f.open {
			f.current = -1
		} else if f.target != f.current && len(f.pending) > 0 && !f.open {
			f.cutOver(emit, now)
		}
	case layer == f.target && f.target >= 0:
		if len(f.pending) == 0 {
			if !keyframeStart(payload) {
				return false
			}
		} else if len(f.pending) >= pendingMost {
			f.pending = nil
			return true
		}
		f.pending = append(f.pending, held{*h, append([]byte(nil), payload...)})
		if f.current < 0 || !f.open || now.Sub(f.lastAt) > silentFor {
			f.cutOver(emit, now)
		}
	default:
		f.late(layer, h, payload, emit)
	}
	return false
}

// late sends a late packet (a retransmission) of a run before the one
// being sent, in the place that run left for it.
func (f *forwarder) late(layer int, h *rtp.Header, payload []byte, emit emitFunc) {
	for i := len(f.past) - 1; i >= 0; i-- {
		if f.past[i].has(layer, h.SequenceNumber) {
			emit(h.SequenceNumber+f.past[i].offset, h, payload)
			return
		}
	}
}

// keep ends the run being sent: its late packets still find their place.
func (f *forwarder) keep() {
	if f.cur.layer < 0 || f.cur.hi == f.cur.lo-1 {
		return // nothing was sent in it
	}
	if len(f.past) == pastMost {
		f.past = append(f.past[:0], f.past[1:]...)
	}
	f.past = append(f.past, f.cur)
}

// pad takes n sequence numbers for padding after the frame last sent, and
// returns the first and the frame's timestamp, for the padding to carry:
// the run being sent carries on past them. Only between frames (the last
// packet sent ended one), and not while switching.
func (f *forwarder) pad(n int) (first uint16, ts uint32, ok bool) {
	if n <= 0 || f.current < 0 || f.open || f.target != f.current {
		return 0, 0, false
	}
	f.keep()
	first = f.lastOut + 1
	f.cur = segment{layer: f.current, lo: f.cur.hi + 1, hi: f.cur.hi, offset: f.cur.offset + uint16(n)}
	f.lastOut += uint16(n)
	return first, f.lastTs, true
}

// send forwards a packet of the current layer.
func (f *forwarder) send(layer int, h *rtp.Header, payload []byte, now time.Time, emit emitFunc) {
	if seqNewer(f.cur.lo, h.SequenceNumber) {
		return // from before this run began
	}
	out := h.SequenceNumber + f.cur.offset
	emit(out, h, payload)
	if seqNewer(out, f.lastOut) {
		f.lastOut, f.lastTs, f.open, f.lastAt = out, h.Timestamp, !h.Marker, now
		f.cur.hi = h.SequenceNumber
	}
}

// cutOver starts sending the target from its held keyframe.
func (f *forwarder) cutOver(emit emitFunc, now time.Time) {
	key := f.pending[0].h
	if f.current >= 0 {
		if tsNewer(f.lastTs, key.Timestamp) {
			f.backs++
		}
		f.keep()
	}
	f.current = f.target
	f.cur = segment{layer: f.current, lo: key.SequenceNumber, hi: key.SequenceNumber - 1, offset: f.lastOut + 1 - key.SequenceNumber}
	f.open = false
	held := f.pending
	f.pending = nil
	for _, p := range held {
		f.send(f.current, &p.h, p.payload, now, emit)
	}
}

// restart forgets layer's place in its stream: the publisher sent it again
// on a new stream, whose sequence numbers start over. If it's the layer
// being sent, the target's next keyframe is waited for (askKey) and cut
// over to at once.
func (f *forwarder) restart(layer int) (askKey bool) {
	f.past = slices.DeleteFunc(f.past, func(s segment) bool { return s.layer == layer })
	if f.target == layer {
		f.pending = nil
	}
	if f.current != layer {
		return false
	}
	f.current, f.open = -1, false
	return f.target >= 0
}
