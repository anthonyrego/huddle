package sfu

// Video through the SFU end to end on loopback: an sfuclient publisher
// sends a camera of three synthetic H.264 layers, and subscribers check
// the layer their settings pick, switches, off and on, keyframe requests,
// sender reports, a layer the publisher pauses, and retransmissions across
// a switch.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"

	rtccodecs "github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/video"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// camLayers are the test camera's layers, largest first, as the room
// publishes a 720p camera; sliceBytes how big each layer's frames are:
// three packets, two and one.
var (
	camLayers = []signal.Layer{
		{RID: "f", W: 1280, H: 720, Bitrate: 1_700_000},
		{RID: "h", W: 640, H: 360, Bitrate: 450_000},
		{RID: "q", W: 320, H: 180, Bitrate: 160_000},
	}
	sliceBytes = []int{3000, 1500, 600}
)

const (
	frameEvery = time.Second / 30
	keyPeriod  = 60 // frames between a layer's own keyframes
)

// camera is a publisher's synthetic camera: every frameEvery, a frame on
// each layer that isn't paused, stamped with the capture clock (90 kHz from
// its start), a keyframe at the layer's first frame, every keyPeriod
// frames after (each layer at its own phase), and on the frame after a PLI
// or FIR asks the layer for one.
type camera struct {
	sid    string
	tracks []*sfuclient.Track // by layer, largest first
	start  time.Time
	stop   chan struct{}
	done   chan struct{}

	mu     sync.Mutex
	plis   [3][]time.Time // keyframe requests each layer heard
	ask    [3]bool
	paused [3]bool
	wrote  [3]int // frames written
	keys   [3]int
	frame  int // the next frame's number
}

// publishCamera publishes c's camera and starts it.
func publishCamera(t *testing.T, c *testClient) *camera {
	t.Helper()
	cam := &camera{start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	cid := "cam-" + c.id
	cam.tracks = make([]*sfuclient.Track, len(camLayers))
	for i, l := range camLayers {
		tr, err := sfuclient.NewTrack(rtccodecs.H264, cid, c.id, l.RID, func(p rtcp.Packet) {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				cam.mu.Lock()
				cam.plis[i] = append(cam.plis[i], time.Now())
				cam.ask[i] = true
				cam.mu.Unlock()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		cam.tracks[i] = tr
	}
	order := slices.Clone(cam.tracks)
	slices.Reverse(order) // the smallest is the transceiver's
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	sid, err := c.s.Publish(ctx, signal.Publish{CID: cid, Kind: signal.Video, Source: signal.Camera, Layers: camLayers}, order...)
	if err != nil {
		t.Fatalf("%s: publish the camera: %v", c.id, err)
	}
	cam.sid = sid
	go cam.run()
	t.Cleanup(cam.halt)
	return cam
}

func (cam *camera) run() {
	defer close(cam.done)
	for n := 0; ; n++ {
		select {
		case <-cam.stop:
			return
		case <-time.After(time.Until(cam.start.Add(time.Duration(n) * frameEvery))):
		}
		ts := uint32(time.Since(cam.start).Seconds() * 90000)
		for i, tr := range cam.tracks {
			cam.mu.Lock()
			if cam.paused[i] {
				cam.mu.Unlock()
				continue
			}
			key := cam.wrote[i] == 0 || (n+20*i)%keyPeriod == 0 || cam.ask[i]
			cam.mu.Unlock()
			if tr.WriteFrame(camFrame(i, n, key), ts, nil) != nil {
				continue // not negotiated yet
			}
			cam.mu.Lock()
			cam.wrote[i]++
			if key {
				cam.keys[i]++
				cam.ask[i] = false
			}
			cam.mu.Unlock()
		}
		cam.mu.Lock()
		cam.frame = n + 1
		cam.mu.Unlock()
	}
}

func (cam *camera) halt() {
	select {
	case <-cam.stop:
	default:
		close(cam.stop)
	}
	<-cam.done
}

// pause stops writing layer i (as a publisher's allocator pauses it), or
// starts again (with an ordinary frame: an encoder that carries on).
func (cam *camera) pause(i int, paused bool) {
	cam.mu.Lock()
	cam.paused[i] = paused
	cam.mu.Unlock()
}

// keyRequests is how many keyframe requests each layer has heard.
func (cam *camera) keyRequests() [3]int {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	return [3]int{len(cam.plis[0]), len(cam.plis[1]), len(cam.plis[2])}
}

// camFrame is frame n of layer i in Annex-B: a keyframe's SPS, PPS and IDR
// slice, or a P slice; the slice names its layer and frame and is filled to
// the layer's size with a byte of its own (no zeros: no start codes).
func camFrame(i, n int, key bool) []byte {
	var b []byte
	if key {
		b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xab, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, 0, 0, 0, 1, 0x65)
	} else {
		b = append(b, 0, 0, 0, 1, 0x41)
	}
	body := make([]byte, sliceBytes[i])
	body[0] = 0x80 | byte(i)
	for k := range 4 {
		body[1+k] = 0x80 | byte(n>>(7*(3-k))&0x7f)
	}
	fill := sliceFill(i, n)
	for k := 5; k < len(body); k++ {
		body[k] = fill
	}
	return append(b, body...)
}

func sliceFill(i, n int) byte { return 0x10 + byte((i*31+n)%200) }

// readSlice reads a slice NAL unit written by camFrame (header byte
// included): its layer and frame, if it's whole.
func readSlice(nal []byte) (layer, frame int, ok bool) {
	if len(nal) < 6 {
		return 0, 0, false
	}
	body := nal[1:]
	layer = int(body[0] & 0x7f)
	if layer >= len(sliceBytes) || len(body) != sliceBytes[layer] {
		return 0, 0, false
	}
	for k := range 4 {
		frame = frame<<7 | int(body[1+k]&0x7f)
	}
	fill := sliceFill(layer, frame)
	for _, x := range body[5:] {
		if x != fill {
			return 0, 0, false
		}
	}
	return layer, frame, true
}

// unit is an access unit a viewer was sent: its packets up to a marker,
// put back together.
type unit struct {
	layer, frame int
	key, whole   bool
	seq0, seq1   uint16
	ts           uint32
	at           time.Time // when its last packet came
}

func (u unit) String() string {
	k := ""
	if u.key {
		k = " key"
	}
	if !u.whole {
		return fmt.Sprintf("{broken %d–%d}", u.seq0, u.seq1)
	}
	return fmt.Sprintf("{%s %d%s}", camLayers[u.layer].RID, u.frame, k)
}

// units puts pkts (in order) back together; a run cut off before its
// marker at the end is left out.
func units(pkts []vidPkt) []unit {
	var out []unit
	start := 0
	for i, p := range pkts {
		if !p.marker {
			continue
		}
		run := pkts[start : i+1]
		start = i + 1
		u := unit{seq0: run[0].seq, seq1: p.seq, ts: p.ts, at: p.at, whole: true}
		dep := &codecs.H264Packet{}
		var au []byte
		for _, q := range run {
			b, err := dep.Unmarshal(q.payload)
			if err != nil {
				u.whole = false
			}
			au = append(au, b...)
		}
		nslices := 0
		var sps, pps bool
		for _, nal := range video.SplitAnnexB(au) {
			switch nal[0] & 0x1f {
			case 7:
				sps = true
			case 8:
				pps = true
			case 1, 5:
				nslices++
				l, f, ok := readSlice(nal)
				u.layer, u.frame, u.whole = l, f, u.whole && ok
				u.key = nal[0]&0x1f == 5 && sps && pps
			}
		}
		u.whole = u.whole && nslices == 1 && (u.key || !sps)
		out = append(out, u)
	}
	return out
}

// videoFrom is a copy of the packets c has heard of who's camera.
func (c *testClient) videoFrom(who string) []vidPkt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h := c.fromLocked(who, signal.Video); h != nil {
		return slices.Clone(h.vids)
	}
	return nil
}

// unitsFrom are the access units c has been sent of who's camera.
func (c *testClient) unitsFrom(who string) []unit { return units(c.videoFrom(who)) }

// settings sends c's settings for who's camera.
func (c *testClient) settings(t *testing.T, who string, w, h int, on bool) {
	t.Helper()
	tr := c.from(who, signal.Video)
	if tr == nil {
		t.Fatalf("%s: no camera of %s", c.id, who)
	}
	if err := c.s.Settings(signal.Settings{SID: tr.sid, W: w, H: h, Enabled: on}); err != nil {
		t.Fatal(err)
	}
}

// checkVideo fails unless us is one stream of whole frames: sequence
// numbers unbroken, the first unit and each layer change a keyframe,
// frames going back only at a layer change (the new layer lagging).
func checkVideo(t *testing.T, what string, us []unit) {
	t.Helper()
	for i, u := range us {
		if !u.whole {
			t.Errorf("%s: unit %d broken: %v", what, i, us[max(0, i-2):min(len(us), i+3)])
			return
		}
		if i == 0 {
			if !u.key {
				t.Errorf("%s: began with %v, not a keyframe", what, u)
			}
			continue
		}
		prev := us[i-1]
		if u.seq0 != prev.seq1+1 {
			t.Errorf("%s: unit %d %v begins at sequence %d after %d", what, i, u, u.seq0, prev.seq1)
			return
		}
		if u.layer != prev.layer && !u.key {
			t.Errorf("%s: %v after %v: a layer change not at a keyframe", what, u, prev)
			return
		}
		if u.layer == prev.layer && u.frame <= prev.frame {
			t.Errorf("%s: %v after %v", what, u, prev)
			return
		}
	}
}

// layersOf counts units by layer, and lists the changes.
func layersOf(us []unit) (string, map[string]int) {
	n := map[string]int{}
	var seq []string
	for i, u := range us {
		rid := camLayers[u.layer].RID
		n[rid]++
		if i == 0 || u.layer != us[i-1].layer {
			seq = append(seq, fmt.Sprintf("%s@%d", rid, u.frame))
		}
	}
	return strings.Join(seq, " → "), n
}

// waitLayers waits until the SFU receives all of who's camera's layers.
func waitLayers(t *testing.T, ts *testSFU, who string, cam *camera) {
	t.Helper()
	waitFor(t, wait, "the SFU to receive every layer of "+who+"'s camera", func() bool {
		r := ts.s.room("r")
		if r == nil {
			return false
		}
		pub := r.participant(who).publication(cam.sid)
		for _, l := range pub.layers {
			if !l.alive.Load() {
				return false
			}
		}
		return true
	})
}

// waitUnits waits until c has been sent n units of who's camera after the
// first of them that's of layer rid ("" any), and returns them all.
func waitUnits(t *testing.T, c *testClient, who, rid string, n int) []unit {
	t.Helper()
	var us []unit
	waitFor(t, wait, fmt.Sprintf("%s to be sent %d units of %s's %s", c.id, n, who, rid), func() bool {
		us = c.unitsFrom(who)
		for i, u := range us {
			if rid == "" || camLayers[u.layer].RID == rid {
				return len(us)-i >= n
			}
		}
		return false
	})
	return us
}

// since are the units from the first after t on.
func since(us []unit, at time.Time) []unit {
	for i, u := range us {
		if u.at.After(at) {
			return us[i:]
		}
	}
	return nil
}

// The layer each viewer is sent follows its settings: the smallest at
// least the size asked for, the largest for a bigger ask, the top for a
// viewer that never says. Switches land within a few frames of the ask, at
// a keyframe, with sequence numbers unbroken; each start and switch asked
// the layer for a keyframe at once.
func TestVideoLayers(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	d := ts.join(t, "r", "d", sfuclient.Options{})
	e := ts.join(t, "r", "e", sfuclient.Options{}) // says nothing
	cam := publishCamera(t, a)
	for _, x := range []*testClient{b, c, d, e} {
		waitFor(t, wait, x.id+" to be offered a's camera", func() bool { return x.from("a", signal.Video) != nil })
	}
	// Every layer running (as it is for anyone who arrives after a
	// camera starts): the first few packets of a publication may come on
	// any layer first, and a viewer is sent what runs meanwhile.
	waitLayers(t, ts, "a", cam)
	asked := time.Now()
	b.settings(t, "a", 1280, 720, true)
	c.settings(t, "a", 342, 192, true) // a tile a little larger than 180p
	d.settings(t, "a", 160, 90, true)
	want := map[*testClient]string{b: "f", c: "h", d: "q", e: "f"}
	for x, rid := range want {
		us := waitUnits(t, x, "a", rid, 30)
		checkVideo(t, x.id, us)
		path, n := layersOf(us)
		first := us[0].at.Sub(asked)
		t.Logf("%s (wants %s): %s, %v; first frame %v after asking", x.id, rid, path, n, first.Round(time.Millisecond))
		if path != rid+"@"+fmt.Sprint(us[0].frame) {
			t.Errorf("%s: sent %s, want %s throughout", x.id, path, rid)
		}
	}
	t.Logf("keyframes asked of each layer: %v", cam.keyRequests())

	// b down to the smallest, c up to the largest.
	switched := time.Now()
	cam.mu.Lock()
	atFrame := cam.frame
	cam.mu.Unlock()
	b.settings(t, "a", 320, 180, true)
	c.settings(t, "a", 1920, 1080, true)
	waitUnits(t, b, "a", "q", 30)
	waitUnits(t, c, "a", "f", 30)
	for x, rid := range map[*testClient]string{b: "q", c: "f"} {
		us := x.unitsFrom("a")
		checkVideo(t, x.id, us)
		path, _ := layersOf(us)
		var landed unit
		for _, u := range since(us, switched) {
			if camLayers[u.layer].RID == rid {
				landed = u
				break
			}
		}
		t.Logf("%s: %s; on %s at frame %d, %d frames and %v after asking at %d", x.id, path, rid, landed.frame,
			landed.frame-atFrame, landed.at.Sub(switched).Round(time.Millisecond), atFrame)
		if landed.frame-atFrame > 5 {
			t.Errorf("%s: switched to %s %d frames after asking", x.id, rid, landed.frame-atFrame)
		}
	}
}

// Off sends nothing; on again starts at a keyframe, the sequence numbers
// carrying on from before.
func TestVideoOffAndOn(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 640, 360, true)
	waitUnits(t, b, "a", "h", 20)
	b.settings(t, "a", 640, 360, false)
	time.Sleep(100 * time.Millisecond) // a frame on its way
	before := len(b.videoFrom("a"))
	time.Sleep(time.Second)
	if n := len(b.videoFrom("a")) - before; n != 0 {
		t.Errorf("%d packets sent while off", n)
	}
	on := time.Now()
	b.settings(t, "a", 640, 360, true)
	waitFor(t, wait, "b to be sent the camera again", func() bool { return len(since(b.unitsFrom("a"), on)) >= 20 })
	us := b.unitsFrom("a")
	checkVideo(t, "b", us)
	again := since(us, on)
	t.Logf("off after %d units; on again at %v, %v after asking", len(us)-len(again), again[0], again[0].at.Sub(on).Round(time.Millisecond))
	if !again[0].key {
		t.Errorf("on again with %v", again[0])
	}

	// Off with a size of nothing, as the app turns off a track it doesn't
	// show.
	b.settings(t, "a", 0, 0, true)
	time.Sleep(100 * time.Millisecond)
	before = len(b.videoFrom("a"))
	time.Sleep(500 * time.Millisecond)
	if n := len(b.videoFrom("a")) - before; n != 0 {
		t.Errorf("%d packets sent at 0×0", n)
	}
}

// A viewer's PLI reaches the layer it's sent, once per keyEvery however
// often it asks; a switch asks the new layer at once.
func TestVideoKeyframeRequests(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 640, 360, true)
	waitUnits(t, b, "a", "h", 10)
	time.Sleep(2 * keyEvery)
	start := cam.keyRequests()
	tr := b.from("a", signal.Video).rt.Track
	asked := time.Now()
	for range 10 {
		if err := b.s.PLI(tr.SSRC()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, wait, "the PLI to reach h", func() bool { return cam.keyRequests()[1] > start[1] })
	cam.mu.Lock()
	took := cam.plis[1][start[1]].Sub(asked)
	cam.mu.Unlock()
	time.Sleep(2 * keyEvery)
	got := cam.keyRequests()
	t.Logf("10 PLIs in 100 ms: the layers heard %v more (the first %v after); before %v", [3]int{got[0] - start[0], got[1] - start[1], got[2] - start[2]}, took.Round(100*time.Microsecond), start)
	if got[0] != start[0] || got[2] != start[2] || got[1]-start[1] != 1 {
		t.Errorf("the layers heard %v, from %v: want one more on h alone", got, start)
	}

	// A switch asks the new layer at once.
	start = cam.keyRequests()
	switched := time.Now()
	b.settings(t, "a", 1280, 720, true)
	waitFor(t, wait, "a keyframe asked of f", func() bool { return cam.keyRequests()[0] > start[0] })
	cam.mu.Lock()
	took = cam.plis[0][start[0]].Sub(switched)
	cam.mu.Unlock()
	t.Logf("switch to f: f asked for a keyframe %v after the settings were sent", took.Round(100*time.Microsecond))
	waitUnits(t, b, "a", "f", 10)
	checkVideo(t, "b", b.unitsFrom("a"))
}

// The SFU's sender reports on a video map RTP time to the publisher's
// clock, before and after a switch (a publication's layers share one
// timestamp base).
func TestVideoSenderReports(t *testing.T) {
	ts := newTestSFU(t, nil)
	var mine srRecorder
	a := ts.join(t, "r", "a", sfuclient.Options{Publish: []interceptor.Factory{&mine}})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 640, 360, true)
	reports := func() []srSeen {
		b.mu.Lock()
		defer b.mu.Unlock()
		return slices.Clone(b.fromLocked("a", signal.Video).srs)
	}
	waitFor(t, 3*wait, "three sender reports on h", func() bool { return len(reports()) >= 3 })
	switchAt := time.Now()
	b.settings(t, "a", 1280, 720, true)
	waitFor(t, 3*wait, "three sender reports after switching to f", func() bool {
		n := 0
		for _, sr := range reports() {
			if sr.at.After(switchAt.Add(200 * time.Millisecond)) {
				n++
			}
		}
		return n >= 3
	})
	srs := reports()
	pubSRs := mine.reports()

	// Each report maps an RTP time to the moment the camera took the frame
	// (its timestamps are its clock), later by the time the report took to
	// reach b's side: a few ms on loopback, and the same for every report.
	var offs []time.Duration
	for _, sr := range srs {
		took := cam.start.Add(time.Duration(float64(sr.rtp) / 90000 * float64(time.Second)))
		offs = append(offs, srTimeAt(sr, sr.rtp, 90000).Sub(took))
	}
	lo, hi := slices.Min(offs), slices.Max(offs)
	t.Logf("%d reports (%d before the switch); against the camera's clock: %v to %v (publisher's own reports: %d)", len(srs),
		len(slices.DeleteFunc(slices.Clone(srs), func(s srSeen) bool { return s.at.After(switchAt) })), lo.Round(100*time.Microsecond), hi.Round(100*time.Microsecond), len(pubSRs))
	for i, o := range offs {
		t.Logf("  report %d at %v: %v", i, srs[i].at.Sub(switchAt).Round(time.Millisecond), o.Round(100*time.Microsecond))
	}
	if lo < -5*time.Millisecond || hi > 25*time.Millisecond || hi-lo > 5*time.Millisecond {
		t.Errorf("the reports map RTP %v to %v from the camera's clock, want 0–25 ms and steady within 5 ms", lo, hi)
	}
}

// srTimeAt is when, by report r, RTP time ts was (clock ticks a second).
func srTimeAt(r srSeen, ts uint32, clock int) time.Time {
	sec, frac := r.ntp>>32, r.ntp&0xffffffff
	at := time.Unix(int64(sec)-ntpEpoch, int64(frac*1e9>>32))
	return at.Add(time.Duration(float64(int32(ts-r.rtp)) / float64(clock) * float64(time.Second)))
}

// A layer the publisher pauses sends its viewers to the next layer down;
// back, it has them back, each change at a keyframe.
func TestVideoLayerPaused(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 1280, 720, true)
	waitUnits(t, b, "a", "f", 20)
	paused := time.Now()
	cam.pause(0, true)
	waitFor(t, wait, "b moved to h", func() bool {
		us := since(b.unitsFrom("a"), paused)
		return len(us) > 0 && camLayers[us[len(us)-1].layer].RID == "h"
	})
	down := since(b.unitsFrom("a"), paused)
	var onH unit
	for _, u := range down {
		if u.layer == 1 {
			onH = u
			break
		}
	}
	time.Sleep(time.Second)
	resumed := time.Now()
	cam.pause(0, false)
	waitFor(t, wait, "b back on f", func() bool {
		us := since(b.unitsFrom("a"), resumed)
		return len(us) > 0 && us[len(us)-1].layer == 0
	})
	var onF unit
	for _, u := range since(b.unitsFrom("a"), resumed) {
		if u.layer == 0 {
			onF = u
			break
		}
	}
	waitUnits(t, b, "a", "", 10)
	us := b.unitsFrom("a")
	checkVideo(t, "b", us)
	path, _ := layersOf(us)
	t.Logf("%s; on h %v after f paused, back on f %v after it resumed", path, onH.at.Sub(paused).Round(time.Millisecond), onF.at.Sub(resumed).Round(time.Millisecond))
	if d := onH.at.Sub(paused); d > layerQuiet+tick+200*time.Millisecond {
		t.Errorf("on h %v after f paused", d)
	}
}

// A viewer's NACKs are answered from what its down track sent, across
// layer switches: with a seventh of what it's sent lost on the way, every
// packet comes again and the stream is whole. (The loss is on the SFU's
// side, before SRTP: a packet dropped after the viewer's SRTP had read it
// would make its resend a replay.)
func TestVideoRetransmissions(t *testing.T) {
	drop := newDropFirst()
	ts := newTestSFU(t, func(s *Server) { s.downAPI = sfuAPI(t, s.net, down, drop) })
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	for _, sz := range [][2]int{{1280, 720}, {320, 180}, {640, 360}, {1280, 720}} {
		b.settings(t, "a", sz[0], sz[1], true)
		time.Sleep(700 * time.Millisecond)
	}
	b.settings(t, "a", 0, 0, false)
	time.Sleep(300 * time.Millisecond)

	pkts := b.videoFrom("a")
	bySeq := map[uint16]vidPkt{}
	for _, p := range pkts {
		bySeq[p.seq] = p
	}
	ordered := slices.Collect(maps.Values(bySeq))
	first := pkts[0].seq
	slices.SortFunc(ordered, func(x, y vidPkt) int { return int(int16(x.seq-first)) - int(int16(y.seq-first)) })
	us := units(ordered)
	// A viewer can't ask again for the first packet it's sent (it doesn't
	// know of it): when that's dropped, the stream begins at the next
	// keyframe, as the app's assembler begins.
	for len(us) > 0 && !us[0].key {
		us = us[1:]
	}
	checkVideo(t, "b", us)
	path, n := layersOf(us)
	drop.mu.Lock()
	dropped := 0
	for _, n := range drop.dropped {
		dropped += n
	}
	drop.mu.Unlock()
	missing := int(ordered[len(ordered)-1].seq-ordered[0].seq) + 1 - len(ordered)
	t.Logf("%s %v: %d packets, %d dropped on the way in, %d missing after resends", path, n, len(ordered), dropped, missing)
	if dropped < 20 || missing != 0 || len(n) != 3 {
		t.Errorf("%d dropped, %d missing, layers %v", dropped, missing, n)
	}
}
