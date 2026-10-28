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
// A voice is read from its TrackRemote and each packet written to every
// down track as it arrives. The track's sender reports are kept, for the
// down tracks' own (downTrack.report). A video's layers are read and
// dropped for now: forwarding them is the media server's step 3.
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

	mu     sync.Mutex
	downs  []*downTrack                   // replaced, never changed in place: forward reads it unlocked
	src    *webrtc.TrackRemote            // the voice being forwarded
	layers map[string]*webrtc.TrackRemote // a video's, by rid
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
		pub.forward(tr, p, levelID, time.Now())
	}
}

// forward writes one packet of the voice to every down track.
func (pub *publication) forward(tr *webrtc.TrackRemote, p *rtp.Packet, levelID uint8, now time.Time) {
	pub.mu.Lock()
	if pub.closed || pub.src != tr {
		pub.mu.Unlock()
		return
	}
	downs := pub.downs
	pub.mu.Unlock()
	var level byte
	var hasLevel bool
	if levelID != 0 {
		if b := p.GetExtension(levelID); len(b) > 0 {
			level, hasLevel = b[0], true
			pub.level.observe(b[0]&0x7f, p.Timestamp, now)
		}
	}
	for _, dt := range downs {
		dt.write(p, level, hasLevel, now)
	}
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

// report is the last sender report.
func (pub *publication) report() senderReport {
	pub.srMu.Lock()
	defer pub.srMu.Unlock()
	return pub.sr
}

// receiveLayer reads one layer of a video and drops it (step 3 forwards
// layers), its RTCP too: pion's interceptors act on what's read, so the
// publisher gets receiver reports and the NACKs it needs.
func (pub *publication) receiveLayer(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	rid := tr.RID()
	known := false
	for _, l := range pub.req.Layers {
		known = known || l.RID == rid
	}
	if !known {
		pub.owner.log.Warn("sfu: a layer that wasn't published", "track", pub.sid, "rid", rid)
		drain(tr, rcv)
		return
	}
	pub.mu.Lock()
	if pub.layers == nil {
		pub.layers = map[string]*webrtc.TrackRemote{}
	}
	pub.layers[rid] = tr
	pub.mu.Unlock()
	pub.owner.log.Debug("sfu: receiving a layer", "track", pub.sid, "rid", rid)
	drain(tr, rcv)
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
