package sfu

// Dynacast: a publisher stops encoding and sending the simulcast layers
// nobody is sent. (LiveKit's, pkg/rtc/dynacast, tells a publisher the
// largest layer anyone is sent, and every layer under it runs; this tells
// it the set itself, so viewers of the smallest and the largest layers
// leave the middle one paused.)
//
// A video's wanted set is, across its down tracks, the layer each
// viewer's settings chose (want: the top one after settingsWait with no
// settings, none while the track is off) and the layer its forwarder is
// sending (current: still sent until a switch to another completes). A
// want on a layer the publisher has paused for bandwidth counts like any
// other: the publisher resumes it when it can, and pick stands in the
// nearest running layer meanwhile. Viewers in-process (the whiteboard)
// are never sent video, so they want nothing.
//
// The publisher hears the set (signal.Wanted) at once when it grows, and
// dynacastShrink after a layer leaves it: a tile that shrinks and grows
// again doesn't stop and restart an encoder. A publisher sends every
// layer until told otherwise, so a video starts with every layer wanted,
// and keeps them dynacastFirst (as LiveKit's first update waits 10 s):
// long enough for the publisher's congestion control to have run every
// layer its uplink carries (the top one comes after a probe, 5–7 s in),
// which lets a layer paused later come back at once when it's wanted (the
// room's allocator resumes a layer without a probe only if it ran). A
// camera nobody watches pauses dynacastFirst after it's published.

import (
	"slices"
	"time"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Dynacast's delays: how long a layer stays wanted after the last viewer
// that wanted it (or was sent it) stops, and after its video is published
// (a var, so tests can shorten it).
const dynacastShrink = 3 * time.Second

var dynacastFirst = 10 * time.Second

// dynacast is what a video publication has told its publisher of the
// layers wanted. Guarded by its publication's dmu.
type dynacast struct {
	until []time.Time // per layer: wanted until then (last in the set, plus dynacastShrink)
	told  []bool      // per layer: wanted, as the publisher was last told (or assumes)
	sent  int         // wanted messages sent
}

func newDynacast(n int, now time.Time) dynacast {
	d := dynacast{until: make([]time.Time, n), told: make([]bool, n)}
	for i := range n {
		d.until[i], d.told[i] = now.Add(dynacastFirst), true
	}
	return d
}

// updateWanted works out the video's wanted set and tells the publisher
// if it changed: a layer joining it at once, one leaving it once it has
// been out of it for dynacastShrink. The room's clock calls it, and so
// does anything that may grow the set (a viewer's settings).
func (pub *publication) updateWanted(now time.Time) {
	if len(pub.layers) == 0 {
		return
	}
	in := make([]bool, len(pub.layers))
	for _, dt := range pub.downTracks() {
		dt.wanted(in)
	}
	pub.dmu.Lock()
	defer pub.dmu.Unlock()
	d := &pub.dyn
	tell := make([]bool, len(in))
	for i, w := range in {
		if until := now.Add(dynacastShrink); w && until.After(d.until[i]) {
			d.until[i] = until
		}
		tell[i] = now.Before(d.until[i])
	}
	if slices.Equal(tell, d.told) {
		return
	}
	pub.mu.Lock()
	closed := pub.closed
	pub.mu.Unlock()
	if closed {
		return
	}
	was := pub.rids(d.told)
	d.told = tell
	d.sent++
	// Sent under dmu, so the publisher hears the sets in the order they
	// were worked out.
	rids := pub.rids(tell)
	pub.owner.send(signal.KindWanted, signal.Wanted{SID: pub.sid, RIDs: rids})
	pub.owner.log.Info("sfu: wanted", "track", pub.sid, "rids", rids, "was", was)
}

// rids names the layers marked in ls, largest first.
func (pub *publication) rids(ls []bool) []string {
	out := []string{}
	for i, w := range ls {
		if w {
			out = append(out, pub.layers[i].info.RID)
		}
	}
	return out
}

// wantedRIDs is the set the publisher was last told (for tests).
func (pub *publication) wantedRIDs() []string {
	pub.dmu.Lock()
	defer pub.dmu.Unlock()
	return pub.rids(pub.dyn.told)
}

// wanted marks in in the layers this viewer wants: the one its settings
// chose and the one it's being sent.
func (d *downTrack) wanted(in []bool) {
	if d.fwd == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, i := range []int{d.want, d.fwd.current} {
		if i >= 0 && i < len(in) {
			in[i] = true
		}
	}
}
