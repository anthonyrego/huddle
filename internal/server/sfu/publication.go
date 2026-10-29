package sfu

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// published counts publications, so a participant's tracks are listed in
// the order they came.
var published atomic.Int64

// publication is one published track: what the SFU receives of it, and the
// down tracks it feeds, one per subscriber.
//
// A voice is read from its TrackRemote (or written by an in-process
// participant's LocalTrack) and each packet written to every down track as
// it arrives. The track's sender reports are kept, for the down tracks'
// own (downTrack.report). A video's layers are each read from a stream of
// their own (layers.go), and every packet goes to every down track, whose
// forwarder sends its viewer one layer.
type publication struct {
	sid, cid string
	n        int64
	owner    *participant
	kind     webrtc.RTPCodecType
	codec    webrtc.RTPCodecCapability
	req      signal.Publish   // as asked; never changed
	info     signal.TrackInfo // guarded by room.mu
	muted    atomic.Bool
	level    audioLevel // a voice's, from the RFC 6464 levels its packets carry

	// layers are a video's, largest first, as published (fixed); lmu
	// guards their streams' SSRCs, sender reports and keyframe requests.
	layers []*layer
	lmu    sync.Mutex

	mu     sync.Mutex
	downs  []*downTrack // replaced, never changed in place: forward reads it unlocked
	src    any          // where the voice being forwarded comes from: a *webrtc.TrackRemote, or a *LocalTrack
	closed bool

	srMu sync.Mutex
	sr   senderReport
}

// senderReport is the publisher's last sender report on the stream being
// forwarded, and when it arrived.
type senderReport struct {
	ok   bool
	ssrc uint32
	rtp  uint32
	at   time.Time
}

func newPublication(owner *participant, sid string, req signal.Publish) *publication {
	pub := &publication{
		sid: sid, cid: req.CID, n: published.Add(1), owner: owner, req: req,
		kind: webrtc.RTPCodecTypeAudio, codec: codecs.Opus,
		info: signal.TrackInfo{SID: sid, Participant: owner.grant.Identity, Kind: req.Kind, Source: req.Source,
			Name: req.Name, Layers: req.Layers, Muted: req.Muted},
	}
	if req.Kind == signal.Video {
		pub.kind, pub.codec = webrtc.RTPCodecTypeVideo, codecs.H264
		pub.layers = newLayers(req)
	}
	pub.muted.Store(req.Muted)
	return pub
}

func (pub *publication) addDown(dt *downTrack) {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if !pub.closed {
		pub.downs = append(slices.Clone(pub.downs), dt)
	}
}

func (pub *publication) removeDown(dt *downTrack) {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	pub.downs = slices.DeleteFunc(slices.Clone(pub.downs), func(d *downTrack) bool { return d == dt })
}

// close stops forwarding (the track is unpublished, or its publisher left).
func (pub *publication) close() {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	pub.closed, pub.downs = true, nil
}

// receive reads one of the track's streams as it arrives on the up
// connection: the voice, or one layer of a video.
func (pub *publication) receive(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	if pub.kind == webrtc.RTPCodecTypeVideo {
		pub.receiveLayer(tr, rcv)
		return
	}
	pub.mu.Lock()
	if pub.src != nil {
		// The publisher sent the track again (a new stream for the same
		// publish): the down tracks carry on from it.
		pub.owner.log.Info("sfu: a track's stream replaced", "track", pub.sid, "ssrc", uint32(tr.SSRC()))
	}
	pub.src = tr
	pub.mu.Unlock()
	var levelID uint8
	for _, e := range rcv.GetParameters().HeaderExtensions {
		if e.URI == sdp.AudioLevelURI {
			levelID = uint8(e.ID)
		}
	}
	go pub.readReports(tr, func() ([]rtcp.Packet, error) { pkts, _, err := rcv.ReadRTCP(); return pkts, err })
	for {
		p, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		var level byte
		var hasLevel bool
		if levelID != 0 {
			if b := p.GetExtension(levelID); len(b) > 0 {
				level, hasLevel = b[0], true
			}
		}
		pub.forward(tr, p, level, hasLevel, time.Now())
	}
}

// forward writes one packet of the voice, from src, to every down track;
// level is its RFC 6464 byte, when hasLevel. False when the publication
// has closed, or src isn't what it forwards.
func (pub *publication) forward(src any, p *rtp.Packet, level byte, hasLevel bool, now time.Time) bool {
	pub.mu.Lock()
	if pub.closed || pub.src != src {
		pub.mu.Unlock()
		return false
	}
	downs := pub.downs
	pub.mu.Unlock()
	if hasLevel {
		pub.level.observe(level&0x7f, p.Timestamp, now)
	}
	for _, dt := range downs {
		dt.write(p, level, hasLevel, now)
	}
	return true
}

// readReports keeps the stream's sender reports until it ends. Reports
// with no packets counted come from a sender that hasn't sent yet, and map
// nothing (pion's sender reports then carry a made-up RTP time). The
// stream's first report is passed on at once to the down tracks already
// sending, which had none to translate.
func (pub *publication) readReports(tr *webrtc.TrackRemote, read func() ([]rtcp.Packet, error)) {
	for {
		pkts, err := read()
		if err != nil {
			return
		}
		now := time.Now()
		for _, pk := range pkts {
			sr, ok := pk.(*rtcp.SenderReport)
			if !ok || sr.SSRC != uint32(tr.SSRC()) || sr.PacketCount == 0 {
				continue
			}
			pub.srMu.Lock()
			first := !pub.sr.ok || pub.sr.ssrc != sr.SSRC
			pub.sr = senderReport{ok: true, ssrc: sr.SSRC, rtp: sr.RTPTime, at: now}
			pub.srMu.Unlock()
			if first {
				pub.mu.Lock()
				downs := pub.downs
				pub.mu.Unlock()
				for _, dt := range downs {
					dt.reportIfDue(now)
				}
			}
		}
	}
}

// setReport stands in for a sender report: an in-process publisher's,
// whose RTP time is now.
func (pub *publication) setReport(sr senderReport) {
	pub.srMu.Lock()
	pub.sr = sr
	pub.srMu.Unlock()
}

// report is the last sender report.
func (pub *publication) report() senderReport {
	pub.srMu.Lock()
	defer pub.srMu.Unlock()
	return pub.sr
}

// drain reads a stream and its RTCP until it ends.
func drain(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	rid := tr.RID()
	go func() {
		for {
			var err error
			if rid != "" {
				_, _, err = rcv.ReadSimulcastRTCP(rid)
			} else {
				_, _, err = rcv.ReadRTCP()
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		if _, _, err := tr.ReadRTP(); err != nil {
			return
		}
	}
}
