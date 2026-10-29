package room

// Sending video: the layers of a source are published as one simulcast
// track (the SFU forwards each receiver the layer that fits it). A
// receiver's keyframe request reaches the layer it was for; the estimate
// is shared among every layer of every video sent.

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/video"
)

// keyEvery is the least time between two keyframes asked for on one layer:
// several receivers asking at once get one.
const keyEvery = 200 * time.Millisecond

// videoPub is one published video.
type videoPub struct {
	r      *room
	src    rtc.VideoSource
	kind   rtc.VideoKind
	layers []rtc.Layer
	out    videoOut
	base   uint32 // the RTP timestamp at pts 0

	lastKey  []atomic.Int64 // per layer: when a keyframe was last asked for (unix ns)
	keys     []atomic.Int64 // per layer: keyframes asked for
	written  []atomic.Int64 // per layer: access units written
	bytes    []atomic.Int64
	keysOut  []atomic.Int64 // per layer: keyframes written
	biggest  []atomic.Int64 // per layer: the largest access unit written
	fill     []atomic.Int64 // per layer: bits a second it must send (a probe's carrier), else 0
	lastPTS  []atomic.Int64
	padBytes []atomic.Int64 // per layer: filler sent for probes

	mu      sync.Mutex
	bps     []int // what congestion control gives each layer
	stopped bool
}

// PublishVideo sends a camera or a screen, a simulcast layer per layer of
// the source.
func (r *room) PublishVideo(src rtc.VideoSource, kind rtc.VideoKind) (rtc.VideoPub, error) {
	layers := src.Layers()
	p := &videoPub{r: r, src: src, kind: kind, layers: layers, base: rand.Uint32() | 1,
		lastKey: make([]atomic.Int64, len(layers)), keys: make([]atomic.Int64, len(layers)),
		written: make([]atomic.Int64, len(layers)), bytes: make([]atomic.Int64, len(layers)),
		keysOut: make([]atomic.Int64, len(layers)), biggest: make([]atomic.Int64, len(layers)),
		fill: make([]atomic.Int64, len(layers)), lastPTS: make([]atomic.Int64, len(layers)), padBytes: make([]atomic.Int64, len(layers)),
		bps: make([]int, len(layers))}
	out, err := r.t.publishVideo(kind, layers, p.rtcp)
	if err != nil {
		return nil, err
	}
	p.out = out
	full := make([]int, len(layers))
	for i, l := range layers {
		full[i] = l.Bitrate
	}
	r.cc.add(p, full)
	if w, ok := out.(wantedOut); ok {
		w.onWanted(func(wanted []bool) { r.cc.setWanted(p, wanted) })
	}
	if err := src.Start(p.write); err != nil {
		r.cc.remove(p)
		out.unpublish()
		return nil, err
	}
	slog.Info("video: publishing", "kind", kind, "track", out.sid(), "layers", layerNames(layers))
	return p, nil
}

func layerNames(ls []rtc.Layer) []string {
	var out []string
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%dx%d@%dk", l.W, l.H, l.Bitrate/1000))
	}
	return out
}

// write sends one access unit of a layer; pts is its capture time on the
// source's clock, which sets its RTP timestamp.
func (p *videoPub) write(layer int, au []byte, pts time.Duration) {
	if layer < 0 || layer >= len(p.layers) {
		return
	}
	ts := p.base + uint32(pts*90000/time.Second)
	au = p.padded(layer, au, pts)
	if err := p.out.write(layer, au, ts); err != nil {
		slog.Debug("video: write", "layer", layer, "err", err)
		return
	}
	p.written[layer].Add(1)
	p.bytes[layer].Add(int64(len(au)))
	if video.IsKeyframe(au) {
		p.keysOut[layer].Add(1)
	}
	for n := int64(len(au)); ; {
		b := p.biggest[layer].Load()
		if n <= b || p.biggest[layer].CompareAndSwap(b, n) {
			break
		}
	}
}

// rtcp is a receiver's feedback on one layer: a keyframe request (PLI, or
// FIR) goes to the source, once per keyEvery.
func (p *videoPub) rtcp(layer int, pkt rtcp.Packet) {
	switch pkt.(type) {
	case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
	default:
		return
	}
	now := time.Now().UnixNano()
	last := p.lastKey[layer].Load()
	if now-last < int64(keyEvery) || !p.lastKey[layer].CompareAndSwap(last, now) {
		return
	}
	p.keys[layer].Add(1)
	p.src.KeyFrame(layer)
}

// setBitrates gives the source what congestion control allows; why says,
// by layer, why each last started or stopped (for the log: "unwanted",
// nobody is sent it; "wanted", back at once when someone was; "bandwidth",
// the estimate paused it; "probed", "started" or "published", it came on
// after a probe, with nothing to probe with, or at publish).
func (p *videoPub) setBitrates(bps []int, why []string) {
	p.mu.Lock()
	if p.stopped || slicesEqual(bps, p.bps) {
		p.mu.Unlock()
		return
	}
	was := p.bps
	p.bps = bps
	p.mu.Unlock()
	for i := range bps {
		if (was[i] == 0) != (bps[i] == 0) {
			slog.Info("video: layer", "kind", p.kind, "layer", layerNames(p.layers[i : i+1])[0], "on", bps[i] > 0, "bps", bps[i], "why", why[i])
		}
	}
	p.src.SetBitrates(bps)
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Stop stops the source and unpublishes.
func (p *videoPub) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()
	p.src.Stop()
	p.r.cc.remove(p)
	if err := p.out.unpublish(); err != nil {
		slog.Debug("video: unpublish", "err", err)
	}
	slog.Info("video: stopped publishing", "kind", p.kind)
}

// PubStats are one published video's numbers.
type PubStats struct {
	Kind    rtc.VideoKind
	Layers  []rtc.Layer
	Bitrate []int    // what congestion control gives each layer now (0: paused)
	Wanted  []bool   // whether the SFU says anyone is sent each layer (Dynacast)
	Paused  []string // why each paused layer is: "unwanted" or "bandwidth" ("-" running)
	Written []int64  // access units sent, per layer
	Bytes   []int64
	Keys    []int64 // keyframes receivers asked for, per layer
	KeysOut []int64 // keyframes sent, per layer
	Biggest []int64 // the largest access unit sent, per layer
	Padding []int64 // filler bytes sent for probes, per layer
}

func (p *videoPub) stats() PubStats {
	p.mu.Lock()
	s := PubStats{Kind: p.kind, Layers: p.layers, Bitrate: append([]int(nil), p.bps...)}
	p.mu.Unlock()
	for i := range p.layers {
		s.Written = append(s.Written, p.written[i].Load())
		s.Bytes = append(s.Bytes, p.bytes[i].Load())
		s.Keys = append(s.Keys, p.keys[i].Load())
		s.KeysOut = append(s.KeysOut, p.keysOut[i].Load())
		s.Biggest = append(s.Biggest, p.biggest[i].Load())
		s.Padding = append(s.Padding, p.padBytes[i].Load())
	}
	return s
}

// videoCC is the room's congestion control for what it sends: the
// publisher connection's estimator, and the allocator that shares it.
type videoCC struct {
	r *room

	mu       sync.Mutex
	est      *icept.Estimator
	alloc    allocator
	pubs     []*videoPub
	raw      int // GCC's estimate
	usable   int // what the layers were given from
	uplinks  []*icept.Link
	stopOnce sync.Once
	quit     chan struct{}
}

func newVideoCC(r *room) *videoCC {
	return &videoCC{r: r, raw: icept.BWEInitial, quit: make(chan struct{})}
}

// estimator is the publisher connection's, once the transport has one.
func (v *videoCC) estimator() *icept.Estimator {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.est != nil {
		return v.est
	}
	e := v.r.t.estimator()
	if e == nil {
		return nil
	}
	v.est = e
	e.OnTargetBitrateChange(func(bps int) {
		v.mu.Lock()
		v.raw = bps
		v.mu.Unlock()
		v.allocate()
	})
	v.raw = e.GetTargetBitrate()
	go v.tick()
	return e
}

// tick shares the estimate out again twice a second (the cap moves with
// what's sent, and holds run out), and logs it every 5 s.
func (v *videoCC) tick() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-v.quit:
			return
		case <-t.C:
		}
		v.allocate()
		if n%10 == 0 {
			v.log()
		}
	}
}

func (v *videoCC) add(p *videoPub, full []int) {
	v.estimator()
	v.mu.Lock()
	v.alloc.add(p, full, time.Now())
	v.pubs = append(v.pubs, p)
	v.mu.Unlock()
	v.allocate()
}

// setWanted takes the server's word on which of p's layers anyone is sent
// (Dynacast), and shares the estimate out again at once: a layer someone
// now wants comes back without waiting for the tick.
func (v *videoCC) setWanted(p *videoPub, wanted []bool) {
	v.mu.Lock()
	v.alloc.want(p, wanted)
	v.mu.Unlock()
	var names []string
	for i, w := range wanted {
		if w && i < len(p.layers) {
			names = append(names, layerNames(p.layers[i : i+1])[0])
		}
	}
	slog.Info("video: wanted", "kind", p.kind, "layers", names)
	v.allocate()
}

func (v *videoCC) remove(p *videoPub) {
	v.mu.Lock()
	v.alloc.remove(p)
	for i, q := range v.pubs {
		if q == p {
			v.pubs = append(v.pubs[:i], v.pubs[i+1:]...)
			break
		}
	}
	v.mu.Unlock()
	v.allocate()
}

// allocate shares the estimate out and tells each source.
func (v *videoCC) allocate() {
	sent := 0
	if e := v.estimator(); e != nil {
		sent = e.Pacer.SentRate()
	}
	v.mu.Lock()
	if len(v.pubs) == 0 {
		v.mu.Unlock()
		return
	}
	v.usable = v.alloc.allocate(v.raw, sent, time.Now())
	type set struct {
		p   *videoPub
		bps []int
		why []string
	}
	var sets []set
	given := audioReserve
	cp, cl, probing := v.alloc.carrier()
	for _, p := range v.pubs {
		bps := v.alloc.bitrates(p, len(p.layers))
		for i := range p.fill {
			fill := 0
			if probing && p == cp && i == cl {
				fill = bps[i]
			}
			p.fill[i].Store(int64(fill))
		}
		for _, b := range bps {
			given += b
		}
		sets = append(sets, set{p, bps, v.alloc.whys(p, len(p.layers))})
	}
	est := v.est
	pace := max(v.alloc.pace(v.raw, sent), given)
	v.mu.Unlock()
	if est != nil {
		est.Pacer.SetRate(pace)
	}
	for _, s := range sets {
		s.p.setBitrates(s.bps, s.why)
	}
}

// Estimate is the congestion control's view now.
type Estimate struct {
	Raw, Usable int           // GCC's estimate; what the layers were given from
	Sent        int           // bits sent over the last second
	Queue       time.Duration // the oldest packet waiting in the pacer
	QueueBytes  int
	UplinkDrops int // packets the test uplink dropped
}

func (v *videoCC) estimate() Estimate {
	var s Estimate
	e := v.estimator()
	if e != nil {
		s.Sent = e.Pacer.SentRate()
		s.Queue, s.QueueBytes = e.Pacer.Queued()
	}
	v.mu.Lock()
	s.Raw, s.Usable = v.raw, v.usable
	for _, u := range v.uplinks {
		s.UplinkDrops += u.Dropped()
	}
	v.mu.Unlock()
	return s
}

func (v *videoCC) log() {
	s := v.estimate()
	v.mu.Lock()
	pubs := append([]*videoPub(nil), v.pubs...)
	v.mu.Unlock()
	for _, p := range pubs {
		ps := v.stats(p)
		slog.Info("video: sending", "kind", ps.Kind, "estimate", s.Raw, "usable", s.Usable, "sent", s.Sent,
			"pacerQueue", s.Queue.Round(time.Millisecond), "layerBps", ps.Bitrate, "wanted", ps.Wanted, "paused", ps.Paused,
			"written", ps.Written, "keyframesAsked", ps.Keys)
	}
}

// stats are p's numbers, with what the allocator says of its layers.
func (v *videoCC) stats(p *videoPub) PubStats {
	ps := p.stats()
	v.mu.Lock()
	ps.Wanted, ps.Paused = v.alloc.states(p, len(p.layers))
	v.mu.Unlock()
	return ps
}

func (v *videoCC) stop() { v.stopOnce.Do(func() { close(v.quit) }) }

// SendStats are what a room sends: the estimate and each video.
type SendStats struct {
	Estimate
	Videos []PubStats
}

// VideoSendStats reads a room's congestion control and published videos
// (for tools: huddle video).
func VideoSendStats(rm rtc.Room) SendStats {
	r, ok := rm.(*room)
	if !ok {
		return SendStats{}
	}
	s := SendStats{Estimate: r.cc.estimate()}
	r.cc.mu.Lock()
	pubs := append([]*videoPub(nil), r.cc.pubs...)
	r.cc.mu.Unlock()
	for _, p := range pubs {
		s.Videos = append(s.Videos, r.cc.stats(p))
	}
	return s
}

func (v *videoCC) addUplink(u *icept.Link) {
	v.mu.Lock()
	v.uplinks = append(v.uplinks, u)
	v.mu.Unlock()
}

// A probe's carrier layer must send what it's given, but an encoder
// spends only the bits its pictures need, so what the encoder doesn't use
// is made up with an SEI filler payload (NAL type 6, SEI message type 3):
// part of the stream as far as the SFU, the pacer and the feedback go,
// ignored by decoders. (Filler-data NAL units, type 12, would be simpler,
// but pion's payloader drops them.)
const (
	nalSEI           = 6
	seiFillerPayload = 3
	maxFiller        = 16 << 10 // bytes added to one access unit at most
)

// padded adds filler to a probe carrier's access unit, up to the bits the
// layer was given for the time since its last one.
func (p *videoPub) padded(layer int, au []byte, pts time.Duration) []byte {
	last := p.lastPTS[layer].Swap(int64(pts) + 1) // 0: none yet
	fill := p.fill[layer].Load()
	if fill == 0 || last == 0 {
		return au
	}
	dt := min(max(pts-time.Duration(last-1), 10*time.Millisecond), 100*time.Millisecond)
	want := int(fill * int64(dt) / int64(time.Second) / 8)
	n := min(want-len(au), maxFiller)
	if n < 16 {
		return au
	}
	out := appendFiller(append(make([]byte, 0, len(au)+n+n/255+8), au...), n)
	p.padBytes[layer].Add(int64(len(out) - len(au)))
	return out
}

// appendFiller appends an SEI NAL unit holding one filler payload of
// about n bytes: start code, header, the message's type and size (a 255
// for each whole 255, then the rest), 0xff bytes, the trailing bits. No
// run of zeros: nothing in it reads as a start code.
func appendFiller(dst []byte, n int) []byte {
	if n%255 == 0 {
		n-- // the last size byte would be 0
	}
	dst = append(dst, 0, 0, 0, 1, nalSEI, seiFillerPayload)
	for s := n; ; s -= 255 {
		if s < 255 {
			dst = append(dst, byte(s))
			break
		}
		dst = append(dst, 0xff)
	}
	for range n {
		dst = append(dst, 0xff)
	}
	return append(dst, 0x80)
}
