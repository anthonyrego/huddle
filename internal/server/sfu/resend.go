package sfu

// The resend gate, on each down connection's estimator.
//
// pion's NACK responder (further from the network than congestion
// control) resends a packet every time a viewer's NACK names it, and a
// viewer's NACK generator names a missing packet every 20 ms until it
// comes, up to ten times. While the viewer's link is saturated, what it's
// missing is stuck behind the queue, every NACK comes before the first
// resend arrives, and each resend joins the queue: one lost packet became
// ten, and a viewer behind 1 Mbps was sent 13 Mbps until it dropped
// everything. So while the link is saturated (GCC's delay-based estimate
// under the video sent), a packet is resent only when its last sending is
// older than the feedback's delay (how long the newest packet acknowledged
// took to come back acknowledged, resendGap at least), and at most
// resendMost times: LiveKit's sequencer has the same rule (no resend
// within two round trips, at most three). Otherwise every NACK is
// answered, as before: a lossy link that isn't full repairs as fast as the
// viewer asks.
//
// The gate filters a NACK in place, before the responder reads it: pion's
// interceptors share one parse of each RTCP read (Attributes'
// GetRTCPPackets keeps it), and congestion control, nearest the network,
// reads it first.

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Resending while saturated.
const (
	resendGap  = 20 * time.Millisecond
	resendMost = 2
	// sendsKept is how many of a stream's packets the gate remembers; the
	// responder keeps nackKept.
	sendsKept = 1024
	// twccKept is how many transport-wide numbers' departures are kept.
	twccKept = 4096
	// baseWindow is how far back the feedback's lowest delay is looked for.
	baseWindow = 10 * time.Second
)

// sending is a packet's last sending, and how many times it has been
// resent.
type sending struct {
	seq     uint16
	at      time.Time
	resends uint8
	ok      bool
}

// gate is what the resend gate knows of one down connection.
type gate struct {
	mu     sync.Mutex
	sends  map[uint32]*[sendsKept]sending // by SSRC
	deps   [twccKept]sending              // by transport-wide number
	delay  time.Duration                  // how long the newest packet acknowledged took to be, as last seen
	owd    int64                          // its one-way delay, µs, less the clocks' difference
	lows   []owdAt                        // the lowest of those, a sample a second, over the last baseWindow
	capped int                            // resends not made while saturated
}

func newGate() *gate { return &gate{sends: map[uint32]*[sendsKept]sending{}} }

// wrote notes a packet going out on a video stream (twcc its
// transport-wide number's extension ID, 0 for none): its first sending,
// or a resend.
func (g *gate) wrote(h *rtp.Header, twcc uint8, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ring := g.sends[h.SSRC]
	if ring == nil {
		ring = &[sendsKept]sending{}
		g.sends[h.SSRC] = ring
	}
	s := &ring[h.SequenceNumber%sendsKept]
	if s.ok && s.seq == h.SequenceNumber {
		s.resends++
		s.at = now
	} else {
		*s = sending{seq: h.SequenceNumber, at: now, ok: true}
	}
	if twcc == 0 {
		return
	}
	var ext rtp.TransportCCExtension
	if b := h.GetExtension(twcc); b != nil && ext.Unmarshal(b) == nil {
		g.deps[ext.TransportSequence%twccKept] = sending{seq: ext.TransportSequence, at: now, ok: true}
	}
}

// feedback reads one transport-wide feedback: how long ago the newest
// packet it says arrived was sent (the resends' wait), and each arrived
// packet's one-way delay (its arrival on the viewer's clock less its
// sending on ours: the clocks' difference, constant, plus the delay), for
// the queue.
func (g *gate) feedback(fb *rtcp.TransportLayerCC, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	arrival := int64(fb.ReferenceTime) * 64_000 // µs on the viewer's clock
	deltas := fb.RecvDeltas
	seq, left := fb.BaseSequenceNumber, int(fb.PacketStatusCount)
	newest, newestOWD, ok := uint16(0), int64(0), false
	one := func(sym uint16) {
		if sym != rtcp.TypeTCCPacketReceivedSmallDelta && sym != rtcp.TypeTCCPacketReceivedLargeDelta {
			seq++
			return
		}
		if len(deltas) == 0 {
			seq++
			return
		}
		arrival += deltas[0].Delta
		deltas = deltas[1:]
		if d := g.deps[seq%twccKept]; d.ok && d.seq == seq {
			newest, newestOWD, ok = seq, arrival-d.at.UnixMicro(), true
		}
		seq++
	}
	for _, c := range fb.PacketChunks {
		switch c := c.(type) {
		case *rtcp.RunLengthChunk:
			for range min(int(c.RunLength), left) {
				one(c.PacketStatusSymbol)
				left--
			}
		case *rtcp.StatusVectorChunk:
			for _, sym := range c.SymbolList {
				if left == 0 {
					break
				}
				one(sym)
				left--
			}
		}
	}
	if !ok {
		return
	}
	g.delay = now.Sub(g.deps[newest%twccKept].at)
	g.owd = newestOWD
	// The lowest one-way delay of each second, for the base the queue
	// stands on.
	if n := len(g.lows); n > 0 && now.Sub(g.lows[n-1].at) < time.Second {
		g.lows[n-1].owd = min(g.lows[n-1].owd, g.owd)
	} else {
		g.lows = append(g.lows, owdAt{g.owd, now})
	}
	for len(g.lows) > 0 && now.Sub(g.lows[0].at) > baseWindow {
		g.lows = g.lows[1:]
	}
}

// owdAt is a one-way delay (µs, less the clocks' difference) at a moment.
type owdAt struct {
	owd int64
	at  time.Time
}

// queue is how far the newest packet's one-way delay stands over the
// lowest of the last baseWindow: the queue in front of the viewer's link,
// which GCC's delay-based part sees only while it grows (a queue that has
// filled and drops what overflows holds still).
func (g *gate) queue() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.lows) == 0 {
		return 0
	}
	base := g.lows[0].owd
	for _, l := range g.lows {
		base = min(base, l.owd)
	}
	return time.Duration(g.owd-base) * time.Microsecond
}

// filter takes out of a NACK, while the link is saturated, the packets
// sent too recently to resend, or resent too often.
func (g *gate) filter(n *rtcp.TransportLayerNack, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ring := g.sends[n.MediaSSRC]
	if ring == nil {
		return
	}
	wait := max(g.delay, resendGap)
	var keep []uint16
	dropped := false
	for i := range n.Nacks {
		n.Nacks[i].Range(func(seq uint16) bool {
			s := ring[seq%sendsKept]
			if s.ok && s.seq == seq && (now.Sub(s.at) < wait || s.resends >= resendMost) {
				dropped = true
				g.capped++
			} else {
				keep = append(keep, seq)
			}
			return true
		})
	}
	if dropped {
		n.Nacks = rtcp.NackPairsFromSequenceNumbers(keep)
	}
}

// WriteRTCP is the estimator's: GCC reads the feedback, the gate notes its
// delay, and a NACK is filtered while the link is saturated.
func (e *downEstimator) WriteRTCP(pkts []rtcp.Packet, a interceptor.Attributes) error {
	if err := e.SendSideBWE.WriteRTCP(pkts, a); err != nil {
		return err
	}
	now := time.Now()
	saturated, checked := false, false
	for _, p := range pkts {
		switch p := p.(type) {
		case *rtcp.TransportLayerCC:
			e.gate.feedback(p, now)
		case *rtcp.TransportLayerNack:
			if !checked {
				sentNow, _ := e.video.rate(now)
				saturated, checked = e.estimate() < sentNow, true
			}
			if saturated {
				e.gate.filter(p, now)
			}
		}
	}
	return nil
}

// resendsCapped is how many resends the gate hasn't made.
func (e *downEstimator) resendsCapped() int {
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	return e.gate.capped
}
