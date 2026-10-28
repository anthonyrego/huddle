package sfu

import (
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// newRunGap is the least a stream that starts again (a new SSRC under the
// same publication) is moved on in time from where the old one stopped: a
// voice's frame.
const newRunGap = 20 * time.Millisecond

// downTrack is one subscriber's copy of one published track, on the
// subscriber's down connection: a webrtc.TrackLocal of our own, whose ID is
// the track's SID and stream ID its publisher's identity (so the client can
// match it to the track message it heard first).
//
// What the publisher sends is written through the binding's WriteStream
// (pion's NACK responder keeps it, and the transport-wide sequence number
// goes on it) with the header made the subscriber's: its payload type and
// SSRC, sequence numbers that run on unbroken even when the publisher's
// stream starts again, and only the header extensions the subscriber
// negotiated (the audio level written under its ID; the transport-wide
// number added by pion). Nothing is written before the publisher sends, or
// before the subscriber's connection is up: no padding, ever.
//
// It writes its own sender reports, translated from the publisher's
// (report): pion's would map RTP time to when the SFU forwarded each
// packet, which would put a person's voice and video out of step by
// however differently they were delayed on the way.
type downTrack struct {
	pub   *publication
	sub   *participant
	clock uint32

	mu      sync.Mutex
	bound   bool
	ssrc    uint32
	pt      uint8
	levelID uint8
	w       webrtc.TrackLocalWriter

	// The run being forwarded: the publisher's stream (inSSRC), and the
	// offsets that carry its sequence numbers and timestamps on from the
	// last run's.
	started bool
	inSSRC  uint32
	seqOff  uint16
	tsOff   uint32
	lastSeq uint16 // the newest sent
	lastTs  uint32
	lastAt  time.Time

	packets, octets uint32
	reports         int
	lastReport      time.Time
}

func newDownTrack(pub *publication, sub *participant) *downTrack {
	return &downTrack{pub: pub, sub: sub, clock: pub.codec.ClockRate}
}

// ID, RID, StreamID and Kind are webrtc.TrackLocal's.
func (d *downTrack) ID() string                { return d.pub.sid }
func (d *downTrack) RID() string               { return "" }
func (d *downTrack) StreamID() string          { return d.pub.owner.grant.Identity }
func (d *downTrack) Kind() webrtc.RTPCodecType { return d.pub.kind }

// Bind is webrtc.TrackLocal's: the track is negotiated on the subscriber's
// connection.
func (d *downTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	var codec webrtc.RTPCodecParameters
	found := false
	for _, c := range ctx.CodecParameters() {
		if strings.EqualFold(c.MimeType, d.pub.codec.MimeType) {
			codec, found = c, true
			break
		}
	}
	if !found {
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
	}
	var levelID uint8
	for _, e := range ctx.HeaderExtensions() {
		if e.URI == sdp.AudioLevelURI {
			levelID = uint8(e.ID)
		}
	}
	d.mu.Lock()
	d.bound, d.ssrc, d.pt, d.levelID, d.w = true, uint32(ctx.SSRC()), uint8(codec.PayloadType), levelID, ctx.WriteStream()
	d.mu.Unlock()
	go d.readRTCP(ctx.RTCPReader())
	d.sub.log.Info("sfu: subscribed", "track", d.pub.sid, "from", d.pub.owner.grant.Identity, "kind", d.pub.kind.String())
	return codec, nil
}

// Unbind is webrtc.TrackLocal's: the track is off the connection.
func (d *downTrack) Unbind(webrtc.TrackLocalContext) error {
	d.mu.Lock()
	d.bound, d.w = false, nil
	d.mu.Unlock()
	return nil
}

// readRTCP reads what the subscriber says about the track until the
// binding ends: pion's interceptors act on RTCP as it's read (the NACK
// responder, reports).
func (d *downTrack) readRTCP(r interceptor.RTCPReader) {
	b := make([]byte, 1500)
	for {
		if _, _, err := r.Read(b, interceptor.Attributes{}); err != nil {
			return
		}
	}
}

// write sends one packet of the publisher's to the subscriber. level is
// the packet's RFC 6464 byte, when hasLevel.
func (d *downTrack) write(p *rtp.Packet, level byte, hasLevel bool, now time.Time) {
	if !d.sub.down.connected() {
		return
	}
	d.mu.Lock()
	if !d.bound {
		d.mu.Unlock()
		return
	}
	seq, ts, ok := d.munge(p, now)
	if !ok {
		d.mu.Unlock()
		return
	}
	h := rtp.Header{Version: 2, Marker: p.Marker, PayloadType: d.pt, SequenceNumber: seq, Timestamp: ts, SSRC: d.ssrc}
	if hasLevel && d.levelID != 0 {
		if err := h.SetExtension(d.levelID, []byte{level}); err != nil {
			d.mu.Unlock()
			return
		}
	}
	d.packets++
	d.octets += uint32(len(p.Payload))
	first := d.packets == 1
	w := d.w
	d.mu.Unlock()
	if _, err := w.WriteRTP(&h, p.Payload); err != nil {
		return
	}
	if first {
		d.report(now)
	}
}

// munge gives a packet its place in what the subscriber is sent; d.mu is
// held. A packet of a new stream (a new SSRC: the publisher sent the track
// again) starts a run that carries on from the last: the next sequence
// number, and a timestamp as far on as time has gone. A packet with no
// payload (padding alone) isn't sent, and the sequence numbers close up
// behind it.
func (d *downTrack) munge(p *rtp.Packet, now time.Time) (uint16, uint32, bool) {
	if !d.started || p.SSRC != d.inSSRC {
		if !d.started {
			d.seqOff = uint16(rand.Uint32()) - p.SequenceNumber
			d.lastSeq = p.SequenceNumber + d.seqOff - 1
		} else {
			gap := max(now.Sub(d.lastAt), newRunGap)
			d.seqOff = d.lastSeq + 1 - p.SequenceNumber
			d.tsOff = d.lastTs + uint32(gap.Seconds()*float64(d.clock)) - p.Timestamp
		}
		d.started, d.inSSRC = true, p.SSRC
	}
	seq, ts := p.SequenceNumber+d.seqOff, p.Timestamp+d.tsOff
	if len(p.Payload) == 0 {
		if seq == d.lastSeq+1 {
			d.seqOff--
		}
		return 0, 0, false
	}
	if seqNewer(seq, d.lastSeq) {
		d.lastSeq, d.lastTs, d.lastAt = seq, ts, now
	}
	return seq, ts, true
}

// reportIfDue sends a sender report if a second has gone since the last
// (or none has gone yet: the publisher's first report came after the
// first packet).
func (d *downTrack) reportIfDue(now time.Time) {
	d.mu.Lock()
	due := d.bound && d.packets > 0 && (d.reports == 0 || now.Sub(d.lastReport) >= srEvery)
	d.mu.Unlock()
	if due {
		d.report(now)
	}
}

// report sends a sender report translated from the publisher's: NTP is the
// SFU's now, RTP the publisher's report's RTP time moved on by the time
// since it arrived (and by the run's offset). Every track of one publisher
// comes over the same connection with the same delay, so their reports
// keep the publisher's timing between them: the app's lip sync holds. No
// report from the publisher yet (or not on the stream being sent), no
// report.
func (d *downTrack) report(now time.Time) {
	sr := d.pub.report()
	d.mu.Lock()
	if !d.bound || d.packets == 0 || !sr.ok || sr.ssrc != d.inSSRC {
		d.mu.Unlock()
		return
	}
	pkt := &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     toNTP(now),
		RTPTime:     sr.rtp + uint32(now.Sub(sr.at).Seconds()*float64(d.clock)) + d.tsOff,
		PacketCount: d.packets,
		OctetCount:  d.octets,
	}
	d.reports++
	d.lastReport = now
	d.mu.Unlock()
	if err := d.sub.down.pc.WriteRTCP([]rtcp.Packet{pkt}); err != nil {
		d.sub.log.Debug("sfu: a sender report", "track", d.pub.sid, "err", err)
	}
}

// ntpEpoch is 1900 to 1970, in seconds.
const ntpEpoch = 2208988800

// toNTP is t as a 64-bit NTP timestamp.
func toNTP(t time.Time) uint64 {
	ns := t.UnixNano()
	sec := uint64(ns/1e9) + ntpEpoch
	frac := (uint64(ns%1e9) << 32) / 1e9
	return sec<<32 | frac
}
