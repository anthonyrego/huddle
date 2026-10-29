package sfu

// A video publication's simulcast layers, as the SFU receives them: each
// read from a stream of its own (by RID), its sender reports kept, whether
// it runs (a publisher pauses the layers its uplink can't carry), and the
// keyframes asked of it.

import (
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Simulcast:
const (
	// layerQuiet is how long a layer may send nothing while another layer
	// of the track sends before it counts as paused, and its viewers move
	// to a layer that runs. A layer's frames come every 33 ms (67 ms for a
	// screen); a keyframe paced out holds up every layer alike.
	layerQuiet = 300 * time.Millisecond
	// keyEvery is the least time between two keyframes asked of one layer
	// (PLIs), whoever asks: viewers starting, switching, turned back on,
	// or asking themselves.
	keyEvery = 200 * time.Millisecond
	// settingsWait is how long a video down track waits for its viewer's
	// first settings before sending the top layer anyway: a client that
	// never says a size (huddle video without -want) is sent what LiveKit
	// sends one, rather than nothing. The app says a size within a frame
	// of the track arriving, so it never waits this long.
	settingsWait = 250 * time.Millisecond
)

// layer is one simulcast layer of a video publication.
type layer struct {
	idx  int
	info signal.Layer // as published: its RID and size

	cur    atomic.Pointer[webrtc.TrackRemote] // the stream being read
	lastAt atomic.Int64                       // the newest packet's arrival, Unix ns; 0 before any
	alive  atomic.Bool                        // running, as far as the SFU can tell

	meter    *rateMeter // what it sends viewers, for their downlinks
	inFiller bool       // the FU-A being read is filler (its reading goroutine's)

	// Guarded by publication.lmu.
	ssrc    uint32
	sr      senderReport
	lastKey time.Time
	keys    int // keyframes asked of it
}

// newLayers are a video publication's layers, largest first, as published.
func newLayers(req signal.Publish) []*layer {
	ls := make([]*layer, len(req.Layers))
	for i, l := range req.Layers {
		ls[i] = &layer{idx: i, info: l, meter: newRateMeter(rateWindow)}
	}
	return ls
}

func (pub *publication) layerByRID(rid string) *layer {
	for _, l := range pub.layers {
		if l.info.RID == rid {
			return l
		}
	}
	return nil
}

// receiveLayer reads one layer of a video as it arrives, and forwards each
// packet to the down tracks; its RTCP too, keeping the sender reports
// (pion's interceptors act on what's read, so the publisher gets receiver
// reports and the NACKs it needs either way).
func (pub *publication) receiveLayer(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	l := pub.layerByRID(tr.RID())
	if l == nil {
		pub.owner.log.Warn("sfu: a layer that wasn't published", "track", pub.sid, "rid", tr.RID())
		drain(tr, rcv)
		return
	}
	pub.lmu.Lock()
	replaced := l.ssrc != 0
	l.ssrc, l.sr = uint32(tr.SSRC()), senderReport{}
	pub.lmu.Unlock()
	l.cur.Store(tr)
	if replaced {
		// The publisher sent the layer again on a new stream: its sequence
		// numbers start over, so its viewers wait for its next keyframe.
		pub.owner.log.Info("sfu: a layer's stream replaced", "track", pub.sid, "rid", l.info.RID, "ssrc", uint32(tr.SSRC()))
		for _, dt := range pub.downTracks() {
			dt.restart(l.idx, time.Now())
		}
	}
	pub.owner.log.Debug("sfu: receiving a layer", "track", pub.sid, "rid", l.info.RID, "ssrc", uint32(tr.SSRC()))
	go pub.readLayerReports(l, tr, rcv)
	for {
		p, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		pub.forwardLayer(l, tr, p, time.Now())
	}
}

// forwardLayer takes one packet of a layer's stream tr: the layer runs,
// and the packet goes to every down track (each forwards what its viewer
// is sent).
func (pub *publication) forwardLayer(l *layer, tr *webrtc.TrackRemote, p *rtp.Packet, now time.Time) {
	if l.cur.Load() != tr {
		return // a stream since replaced
	}
	l.lastAt.Store(now.UnixNano())
	// A publisher's probe filler, for its uplink: viewers are sent its
	// sequence numbers and marker in a byte of padding, not its bytes.
	if filler(p.Payload, &l.inFiller) {
		p.Payload, p.Padding, p.PaddingSize = fillerStub, true, 0
	} else {
		p.Padding, p.PaddingSize = false, 0 // any padding was read off already
	}
	l.meter.add(now, len(p.Payload)+rtpOverhead)
	if !l.alive.Load() && l.alive.CompareAndSwap(false, true) {
		// A layer starting again opens with a keyframe (a new encoder) as
		// often as not, and this packet may be it: viewers moving to it ask
		// for one only if its packets turn out not to start one.
		pub.owner.log.Info("sfu: a layer runs", "track", pub.sid, "rid", l.info.RID)
		pub.layersChanged(now, false)
	}
	pub.mu.Lock()
	if pub.closed {
		pub.mu.Unlock()
		return
	}
	downs := pub.downs
	pub.mu.Unlock()
	for _, dt := range downs {
		dt.writeVideo(l.idx, p, now)
	}
}

// checkLayers marks paused any layer that has sent nothing for layerQuiet
// while another of the track's layers sent (the room's clock calls it).
// When every layer is quiet alike (a still screen, a publisher gone
// quiet), none counts as paused.
func (pub *publication) checkLayers(now time.Time) {
	var newest int64
	for _, l := range pub.layers {
		newest = max(newest, l.lastAt.Load())
	}
	changed := false
	for _, l := range pub.layers {
		if l.alive.Load() && newest-l.lastAt.Load() > int64(layerQuiet) && l.alive.CompareAndSwap(true, false) {
			pub.owner.log.Info("sfu: a layer paused", "track", pub.sid, "rid", l.info.RID)
			changed = true
		}
	}
	if changed {
		pub.layersChanged(now, true)
	}
}

// layersChanged has every viewer's layer chosen again: one has started or
// stopped running. ask: a viewer moved asks its new layer for a keyframe
// at once.
func (pub *publication) layersChanged(now time.Time, ask bool) {
	for _, dt := range pub.downTracks() {
		dt.retarget(now, ask)
	}
}

func (pub *publication) downTracks() []*downTrack {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	return pub.downs
}

// layerFor is the layer a viewer showing the track at w×h pixels wants:
// the smallest at least that size, or the largest if none is. A layer
// whose size wasn't published counts as none.
func (pub *publication) layerFor(w, h int) int {
	for i := len(pub.layers) - 1; i >= 0; i-- {
		if li := pub.layers[i].info; li.W >= w && li.H >= h {
			return i
		}
	}
	return 0
}

// pick is the layer a viewer wanting layer want is sent: want if it runs,
// else the nearest smaller one that runs, else the nearest larger; want
// itself when none runs (whichever comes first is waited for). -1 is none.
func (pub *publication) pick(want int) int {
	if want < 0 || len(pub.layers) == 0 {
		return -1
	}
	want = min(want, len(pub.layers)-1)
	if pub.layers[want].alive.Load() {
		return want
	}
	for i := want + 1; i < len(pub.layers); i++ {
		if pub.layers[i].alive.Load() {
			return i
		}
	}
	for i := want - 1; i >= 0; i-- {
		if pub.layers[i].alive.Load() {
			return i
		}
	}
	return want
}

// askKey asks the publisher for a keyframe on layer idx (a PLI for its
// stream, on the publisher's up connection), unless one was asked for in
// the last keyEvery or the layer hasn't arrived.
func (pub *publication) askKey(idx int, now time.Time) {
	if idx < 0 || idx >= len(pub.layers) || pub.owner.up == nil {
		return
	}
	l := pub.layers[idx]
	pub.lmu.Lock()
	if l.ssrc == 0 || now.Sub(l.lastKey) < keyEvery {
		pub.lmu.Unlock()
		return
	}
	l.lastKey = now
	l.keys++
	ssrc := l.ssrc
	pub.lmu.Unlock()
	if err := pub.owner.up.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{SenderSSRC: ssrc, MediaSSRC: ssrc}}); err != nil {
		pub.owner.log.Debug("sfu: a keyframe request", "track", pub.sid, "rid", l.info.RID, "err", err)
	}
}

// readLayerReports keeps a layer's sender reports until its stream ends
// (those counting no packets map nothing: pion makes up their RTP time).
// The layer's first report goes on at once to the down tracks sending it,
// which had none to translate.
func (pub *publication) readLayerReports(l *layer, tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	for {
		pkts, _, err := rcv.ReadSimulcastRTCP(l.info.RID)
		if err != nil {
			return
		}
		now := time.Now()
		for _, pk := range pkts {
			sr, ok := pk.(*rtcp.SenderReport)
			if !ok || sr.SSRC != uint32(tr.SSRC()) || sr.PacketCount == 0 {
				continue
			}
			pub.lmu.Lock()
			first := !l.sr.ok
			l.sr = senderReport{ok: true, ssrc: sr.SSRC, rtp: sr.RTPTime, at: now}
			pub.lmu.Unlock()
			if first {
				for _, dt := range pub.downTracks() {
					dt.reportIfDue(now)
				}
			}
		}
	}
}

// layerReport is layer idx's last sender report.
func (pub *publication) layerReport(idx int) senderReport {
	if idx < 0 || idx >= len(pub.layers) {
		return senderReport{}
	}
	pub.lmu.Lock()
	defer pub.lmu.Unlock()
	return pub.layers[idx].sr
}
