package sfu

// A video's down track: which simulcast layer its viewer is sent, and the
// keyframes asked for on the way.
//
// The viewer's settings say the size it shows the track at (layerFor: the
// smallest layer at least that size), or off. A track starts off and waits
// for its first settings, or settingsWait after it goes on the connection,
// then sends the top layer. The layer sent is the one wanted if it runs,
// else the nearest that does (pick): a publisher pauses its larger layers
// when its uplink narrows, and its viewers move down and back up with it.
//
// A switch (a start, a size, turned back on, a layer paused) asks the new
// layer for a keyframe at once, and again every keyEvery while the viewer
// waits for it (a layer starting again is asked only if its packets don't
// open with one, as a new encoder's do); the forwarder cuts over at its
// first packet once the old layer has finished its frame. A viewer's own PLI or FIR goes to
// the layer it's sent. The publication throttles what reaches a publisher
// to one request per layer per keyEvery, whoever asks.

import (
	"time"

	"github.com/pion/rtp"
)

// writeVideo takes one packet of the publication's layer, and sends what
// the viewer gets of it.
func (d *downTrack) writeVideo(layer int, p *rtp.Packet, now time.Time) {
	if !d.sub.down.connected() {
		return
	}
	d.mu.Lock()
	f := d.fwd
	if !d.bound || (f.target < 0 && f.current < 0) {
		d.mu.Unlock()
		return
	}
	before := d.packets
	again := f.push(layer, &p.Header, p.Payload, now, d.emit)
	if d.packets > before {
		d.padLocked(now) // a probe's padding, after a frame
	}
	target := f.target
	// The target's keyframe hasn't begun: ask for it (again) as the
	// target sends what can't be cut over to.
	waiting := target >= 0 && target != f.current && len(f.pending) == 0 && layer == target
	first := before == 0 && d.packets > 0
	d.mu.Unlock()
	if again || waiting {
		d.pub.askKey(target, now)
	}
	if first {
		d.report(now)
	}
}

// emitLocked sends one packet the forwarder passes on, as the viewer's
// stream: its payload type and SSRC, the forwarder's sequence number, the
// publisher's timestamp and marker (and padding bit: a filler's stand-in,
// layers.go). d.mu is held, so packets go out in the order they're
// numbered.
func (d *downTrack) emitLocked(seq uint16, h *rtp.Header, payload []byte) {
	out := rtp.Header{Version: 2, Padding: h.Padding, Marker: h.Marker, PayloadType: d.pt, SequenceNumber: seq, Timestamp: h.Timestamp, SSRC: d.ssrc}
	if _, err := d.w.WriteRTP(&out, payload); err != nil {
		return
	}
	d.packets++
	d.octets += uint32(len(payload))
}

// settings takes what the viewer said of the track: shown at w×h pixels,
// or off (on false, or no size).
func (d *downTrack) settings(w, h int, on bool, now time.Time) {
	want := -1
	if on && w > 0 && h > 0 {
		want = d.pub.layerFor(w, h)
	}
	d.mu.Lock()
	d.asked, d.want = true, want
	d.mu.Unlock()
	d.sub.log.Debug("sfu: settings", "track", d.pub.sid, "w", w, "h", h, "on", on, "wants", d.ridOf(want))
	d.retarget(now, true)
}

// tick is the room's clock: a viewer that hasn't said a size settingsWait
// after the track went on its connection is sent the top layer.
func (d *downTrack) tick(now time.Time) {
	if d.fwd == nil {
		return
	}
	d.mu.Lock()
	due := !d.asked && d.bound && now.Sub(d.boundAt) >= settingsWait
	if due {
		d.asked, d.want = true, 0
	}
	d.mu.Unlock()
	if due {
		d.sub.log.Debug("sfu: no settings; the top layer", "track", d.pub.sid)
		d.retarget(now, true)
	}
}

// retarget chooses the layer the viewer is sent, from what it wants and
// what runs; a new one is asked for a keyframe at once if ask (and the
// track can send), else by the first of its packets that doesn't start
// one (writeVideo).
func (d *downTrack) retarget(now time.Time, ask bool) {
	d.mu.Lock()
	target := d.pub.pick(d.limited(d.want))
	key := d.fwd.setTarget(target)
	changed := target != d.aimed
	from := d.aimed
	d.aimed = target
	ready := d.bound
	d.mu.Unlock()
	if changed {
		d.sub.log.Info("sfu: video layer", "track", d.pub.sid, "from", d.pub.owner.grant.Identity, "layer", d.ridOf(target), "was", d.ridOf(from))
	}
	if key && ask && ready && d.sub.down.connected() {
		d.pub.askKey(target, now)
	}
}

// restart is a layer's stream started over (new sequence numbers): if the
// viewer is sent it, it waits for the new stream's keyframe.
func (d *downTrack) restart(layer int, now time.Time) {
	if d.fwd == nil {
		return
	}
	d.mu.Lock()
	key := d.fwd.restart(layer)
	d.mu.Unlock()
	if key {
		d.pub.askKey(layer, now)
	}
}

// viewerAsksKey is the viewer's PLI or FIR: the layer it's sent (or about
// to be) is asked for a keyframe.
func (d *downTrack) viewerAsksKey(now time.Time) {
	d.mu.Lock()
	layer := d.fwd.current
	if t := d.fwd.target; t >= 0 && t != layer {
		layer = t
	}
	d.mu.Unlock()
	d.pub.askKey(layer, now)
}

// ridOf names layer i for the log ("" for none).
func (d *downTrack) ridOf(i int) string {
	if i < 0 || i >= len(d.pub.layers) {
		return ""
	}
	return d.pub.layers[i].info.RID
}
