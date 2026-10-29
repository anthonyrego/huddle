package sfu

// The downlink end to end on loopback, and its allocation and padding on
// their own.

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// mediaUnits are the access units c has been sent of who's camera, its
// padding left out.
func mediaUnits(c *testClient, who string) []unit { return units(mediaOnly(c.videoFrom(who))) }

// lastLayer is the layer of the newest unit c has been sent of who's
// camera (-1 none).
func lastLayer(c *testClient, who string) int {
	us := mediaUnits(c, who)
	if len(us) == 0 {
		return -1
	}
	return us[len(us)-1].layer
}

// mediaOnly drops padding (packets with no payload) from pkts.
func mediaOnly(pkts []vidPkt) []vidPkt {
	return slices.DeleteFunc(slices.Clone(pkts), func(p vidPkt) bool { return len(p.payload) == 0 })
}

// A viewer behind a narrow downlink (500 kbps on its receive connection,
// shared by everything it's sent) wants the camera's top layer, as does an
// unlimited one. The limited viewer starts at it, its downlink is limited
// by GCC's first drop and moves it down to the layer that fits, at a
// keyframe; widened again, a probe carries it back up. The other viewer
// keeps the top layer throughout, every frame whole.
func TestDownlinkLimitedViewer(t *testing.T) {
	if testing.Short() {
		t.Skip("25 s of video")
	}
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	var link *icept.RecvLink
	narrow := &icept.Downlink{BPS: 500_000, Slots: icept.UplinkSlots, Made: func(l *icept.RecvLink) { link = l }}
	b := ts.join(t, "r", "b", sfuclient.Options{Subscribe: []interceptor.Factory{narrow}, NoReplayProtection: true})
	c := ts.join(t, "r", "c", sfuclient.Options{})
	cam := publishCamera(t, a)
	for _, x := range []*testClient{b, c} {
		waitFor(t, wait, x.id+" to be offered a's camera", func() bool { return x.from("a", signal.Video) != nil })
	}
	waitLayers(t, ts, "a", cam)
	start := time.Now()
	b.settings(t, "a", 1280, 720, true)
	c.settings(t, "a", 1280, 720, true)

	waitFor(t, 2*wait, "b moved down to h", func() bool {
		us := c.unitsFrom("a")
		return len(us) > 0 && lastLayer(b, "a") == 1
	})
	down := time.Since(start)
	var firstF time.Time
	for _, u := range mediaUnits(b, "a") {
		if u.layer == 0 {
			firstF = u.at
			break
		}
	}
	// Some seconds on h through the narrow link, then the link widens.
	time.Sleep(4 * time.Second)
	held := since(mediaUnits(b, "a"), start.Add(down+500*time.Millisecond))
	widened := time.Now()
	link.SetRate(0)
	deadline := time.Now().Add(30 * time.Second)
	for {
		us := since(mediaUnits(b, "a"), widened)
		if len(us) > 0 && us[len(us)-1].layer == 0 {
			break
		}
		if time.Now().After(deadline) {
			all := mediaUnits(b, "a")
			path, n := layersOf(all)
			t.Fatalf("b not back on f 30 s after the link widened: %s %v; the last %v", path, n, all[max(0, len(all)-5):])
		}
		time.Sleep(5 * time.Millisecond)
	}
	up := time.Since(widened)
	waitUnits(t, b, "a", "", 10)

	bus := mediaUnits(b, "a")
	path, n := layersOf(bus)
	t.Logf("b, behind 500 kbps: %s %v; first f frame %v after asking, on h %v after asking; widened, back on f %v later; the link dropped %d",
		path, n, firstF.Sub(start).Round(time.Millisecond), down.Round(time.Millisecond), up.Round(time.Millisecond), link.Dropped())
	for _, u := range held {
		if u.layer != 1 {
			t.Errorf("b on %s while the link was narrow, after moving down", camLayers[u.layer].RID)
			break
		}
	}
	if down > 2*time.Second+frameEvery {
		t.Errorf("b moved down %v after asking: want within 2 s", down)
	}

	cus := c.unitsFrom("a")
	checkVideo(t, "c", cus)
	cpath, cn := layersOf(cus)
	var gap time.Duration
	for i := 1; i < len(cus); i++ {
		gap = max(gap, cus[i].at.Sub(cus[i-1].at))
	}
	t.Logf("c, unlimited: %s %v, the longest gap between frames %v", cpath, cn, gap.Round(time.Millisecond))
	if cpath != "f@"+fmt.Sprint(cus[0].frame) {
		t.Errorf("c: sent %s, want f throughout", cpath)
	}
	if gap > 250*time.Millisecond {
		t.Errorf("c: a %v gap between frames", gap)
	}
}

// A probe's padding goes between frames, numbered in the stream: with a
// seventh of what the viewer is sent dropped on the way (padding too),
// every packet comes again on its NACK, the stream is unbroken, each run
// of padding follows a frame's marker with that frame's timestamp, and
// the frames are whole, across a layer switch.
func TestDownlinkPadding(t *testing.T) {
	drop := newDropFirst()
	ts := newTestSFU(t, func(s *Server) { s.downAPI = sfuAPI(t, s.net, down, drop) })
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	waitLayers(t, ts, "a", cam)
	b.settings(t, "a", 640, 360, true)
	waitUnits(t, b, "a", "h", 10)
	dt := ts.s.room("r").participant("b").downs[cam.sid]
	dt.padRate.Store(400_000) // as a probe's carrier
	time.Sleep(time.Second)
	b.settings(t, "a", 1280, 720, true)
	time.Sleep(time.Second)
	dt.padRate.Store(0)
	time.Sleep(200 * time.Millisecond)
	b.settings(t, "a", 0, 0, false)
	time.Sleep(300 * time.Millisecond)

	pkts := b.videoFrom("a")
	bySeq := map[uint16]vidPkt{}
	for _, p := range pkts {
		bySeq[p.seq] = p
	}
	first := pkts[0].seq
	ordered := slices.Collect(maps.Values(bySeq))
	slices.SortFunc(ordered, func(x, y vidPkt) int { return int(int16(x.seq-first)) - int(int16(y.seq-first)) })
	missing := int(ordered[len(ordered)-1].seq-ordered[0].seq) + 1 - len(ordered)
	pads, runs, bad := 0, 0, 0
	for i, p := range ordered {
		if len(p.payload) > 0 {
			continue
		}
		pads++
		prev := ordered[i-1]
		if len(prev.payload) > 0 {
			runs++
			if !prev.marker {
				bad++
				t.Errorf("padding %d after packet %d, which didn't end a frame", p.seq, prev.seq)
			}
		}
		if p.ts != prev.ts {
			bad++
			t.Errorf("padding %d at timestamp %d after %d", p.seq, p.ts, prev.ts)
		}
		if bad > 3 {
			break
		}
	}
	us := units(mediaOnly(ordered))
	for len(us) > 0 && !us[0].key {
		us = us[1:] // the first packet sent can't be asked for again
	}
	for i, u := range us {
		if !u.whole {
			t.Fatalf("unit %d broken: %v", i, us[max(0, i-2):min(len(us), i+3)])
		}
		if i > 0 && u.layer != us[i-1].layer && !u.key {
			t.Fatalf("%v after %v: a layer change not at a keyframe", u, us[i-1])
		}
	}
	drop.mu.Lock()
	dropped := 0
	for _, n := range drop.dropped {
		dropped += n
	}
	drop.mu.Unlock()
	path, n := layersOf(us)
	t.Logf("%s %v: %d packets, %d of them padding in %d runs between frames, %d dropped on the way, %d missing after resends", path, n,
		len(ordered), pads, runs, dropped, missing)
	if missing != 0 || pads < 100 || runs < 20 || len(n) != 2 {
		t.Errorf("%d missing, %d padding in %d runs, layers %v", missing, pads, runs, n)
	}
}

// A publisher's probe fills a layer with SEI filler after each frame: a
// viewer is sent each packet of it as a byte of padding, its sequence
// number and the frame's marker kept, so the frames are whole and the
// stream unbroken, and the layer is measured without it.
func TestFillerStripped(t *testing.T) {
	ts := newTestSFU(t, nil)
	a := ts.join(t, "r", "a", sfuclient.Options{})
	b := ts.join(t, "r", "b", sfuclient.Options{})
	cam := publishCamera(t, a)
	waitFor(t, wait, "b to be offered a's camera", func() bool { return b.from("a", signal.Video) != nil })
	b.settings(t, "a", 640, 360, true)
	waitUnits(t, b, "a", "h", 10)
	cam.mu.Lock()
	cam.filler = 3000 // three packets more a frame
	cam.mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	pkts := b.videoFrom("a")
	stubs, markers := 0, 0
	for _, p := range pkts {
		if len(p.payload) == 0 {
			stubs++
			if p.marker {
				markers++
			}
		}
	}
	us := b.unitsFrom("a")
	checkVideo(t, "b", us)
	pub := ts.s.room("r").participant("a").publication(cam.sid)
	rate := pub.layerRate(1, time.Now())
	t.Logf("%d packets, %d of them filler's stand-ins (%d with a frame's marker), %d whole units; h measured at %d kbps", len(pkts), stubs, markers, len(us), rate/1000)
	if stubs < 60 || markers < 20 {
		t.Errorf("%d stand-ins, %d markers among them", stubs, markers)
	}
	if rate > 600_000 {
		t.Errorf("h measured at %d kbps with the filler: it's sent as padding", rate/1000)
	}
}

// The allocation's order: a screen first, then the larger tiles, then the
// smaller; each video's smallest layer before any is raised, so the
// lowest-priority video goes off first; a layer being sent kept at
// keepShare of its rate; none raised past what it has or its viewer
// wants; a video starting held to the capped budget.
func TestFitVideos(t *testing.T) {
	cam := []int{1_500_000, 450_000, 150_000}
	scr := []int{2_500_000, 800_000}
	n := int64(0)
	video := func(source signal.Source, rates []int, area int64, want, lim, aimed int) dlTrack {
		n++
		pub := &publication{n: n, req: signal.Publish{Source: source}, layers: make([]*layer, len(rates))}
		for i, r := range rates {
			pub.layers[i] = &layer{info: signal.Layer{Bitrate: r}}
		}
		v := dlTrack{dt: &downTrack{pub: pub}, want: want, lim: lim, aimed: aimed, screen: source == signal.Screen, area: area}
		for i := range rates {
			v.running = append(v.running, i)
		}
		return v
	}
	rate := func(dt *downTrack, i int) int { return dt.pub.layers[i].info.Bitrate }
	names := func(vs []dlTrack, got []int) string {
		var out []string
		for k, v := range vs {
			out = append(out, fmt.Sprintf("%d:%d", v.dt.pub.n, got[k]))
		}
		return fmt.Sprint(out)
	}
	big := video(signal.Camera, cam, 640*360, 0, 0, 0)
	small := video(signal.Camera, cam, 320*180, 0, 0, 0)
	screen := video(signal.Screen, scr, 1920*1080, 0, 0, 0)
	vs := []dlTrack{small, big, screen}
	sortVideos(vs)
	if vs[0].dt != screen.dt || vs[1].dt != big.dt || vs[2].dt != small.dt {
		t.Fatalf("in order %v, want the screen (3), the big camera (2), the small one (1)", names(vs, make([]int, 3)))
	}
	for _, c := range []struct {
		name       string
		vs         []dlTrack
		keep, grow int
		want       []int
	}{
		// Everything at its top layer, sent: kept at 3/4 of each.
		{"room for all", []dlTrack{screen, big, small}, 5_000_000, 5_000_000, []int{0, 0, 0}},
		// 1.6 Mbps: the screen at its half size, then the big camera and
		// the small one each at 180p, then raised in order: the big to
		// 360p, and nothing left to raise the small one.
		{"1.6 Mbps", []dlTrack{screen, big, small}, 1_600_000, 1_600_000, []int{1, 1, 2}},
		// 1 Mbps: the screen's half size and the big camera's 180p; the
		// small camera, last, goes off.
		{"1 Mbps", []dlTrack{screen, big, small}, 1_000_000, 1_000_000, []int{1, 2, -1}},
		// 1.2 Mbps, the camera at 720p kept while 3/4 of it fits.
		{"kept", []dlTrack{big}, 1_200_000, 0, []int{0}},
		{"not kept", []dlTrack{big}, 1_100_000, 0, []int{1}},
		// Held at 360p: not raised without a probe, though it fits.
		{"no higher", []dlTrack{video(signal.Camera, cam, 640*360, 0, 1, 1)}, 5_000_000, 5_000_000, []int{1}},
		// Wanting 360p: no higher, though 720p fits.
		{"what it wants", []dlTrack{video(signal.Camera, cam, 640*360, 1, 0, 1)}, 5_000_000, 5_000_000, []int{1}},
		// Off: stays off (it comes back through a probe).
		{"off", []dlTrack{video(signal.Camera, cam, 640*360, 0, limOff, -1)}, 5_000_000, 5_000_000, []int{-1}},
		// Starting: the capped budget says 360p, though GCC's estimate
		// would carry 720p.
		{"starting", []dlTrack{video(signal.Camera, cam, 640*360, 0, limNew, -1)}, 5_000_000, 600_000, []int{1}},
	} {
		got := fitVideos(c.vs, c.keep, c.grow, keepShare, rate)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// While the link is overused, the layer being sent is kept only if all
	// of it fits.
	if got := fitVideos([]dlTrack{big}, 1_200_000, 0, 1, rate); !slices.Equal(got, []int{1}) {
		t.Errorf("overused at 1.2 Mbps: %v, want [1]", got)
	}
}

// The forwarder's padding takes sequence numbers after the frame last
// sent, only between frames and not while switching, and a late packet of
// the run before it still goes out in its place.
func TestForwarderPadding(t *testing.T) {
	s := &synth{}
	f := newForwarder(100)
	var out []sent
	emit := func(seq uint16, _ *rtp.Header, payload []byte) {
		out = append(out, sent{seq, tagOf(payload), keyframeStart(payload)})
	}
	now := time.Unix(0, 0)
	f.setTarget(1)
	var late rtp.Packet
	pads := map[uint16]bool{}
	for n := range 20 {
		for i, p := range s.frame(1, n) {
			if n == 5 && i == 1 {
				late = p // held back: resent after the padding
				continue
			}
			f.push(1, &p.Header, p.Payload, now, emit)
			if !p.Marker {
				if _, _, ok := f.pad(2); ok {
					t.Fatalf("frame %d: padding inside a frame", n)
				}
			}
		}
		if first, _, ok := f.pad(2); ok {
			pads[first], pads[first+1] = true, true
		} else {
			t.Fatalf("frame %d: no padding after it", n)
		}
		if n == 7 {
			f.push(1, &late.Header, late.Payload, now, emit)
		}
		now = now.Add(frameEvery)
	}
	// A switch waiting for its keyframe: no padding meanwhile.
	f.setTarget(2)
	if _, _, ok := f.pad(1); ok {
		t.Fatal("padding while switching")
	}
	got := map[uint16]sent{}
	for _, o := range out {
		got[o.seq] = o
	}
	lo := out[0].seq
	var stream []sent
	for seq := lo; seq != f.lastOut+1; seq++ {
		if o, ok := got[seq]; ok {
			o.seq = uint16(len(stream)) // the frames, in sequence order, with the padding out
			stream = append(stream, o)
		} else if !pads[seq] {
			t.Fatalf("sequence %d neither sent nor padding", seq)
		}
	}
	checkStream(t, stream)
	t.Logf("%d packets sent and %d padding, %d to %d, unbroken; the late packet in its place", len(out), len(pads), lo, f.lastOut)
}
