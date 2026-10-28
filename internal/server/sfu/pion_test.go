package sfu

// What the SFU relies on pion for, pion to pion over loopback: simulcast
// layers told apart by rid (one of them starting late), retransmission
// without RTX, a PLI reaching the layer it names, sender reports per layer,
// mid and rid dropped once acknowledged, a screen added beside a camera,
// remote tracks announced before their first packet, and one UDP port for
// every connection.

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
)

func testNet(t *testing.T) *network {
	t.Helper()
	n, err := listen(Network{IP: "127.0.0.1", Loopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.close() })
	return n
}

func testPC(t *testing.T, api *webrtc.API) *webrtc.PeerConnection {
	t.Helper()
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc
}

func sfuAPI(t *testing.T, n *network, r role, extra ...interceptor.Factory) *webrtc.API {
	t.Helper()
	api, err := newAPI(n, r, extra...)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func clientAPI(t *testing.T, o sfuclient.Options, publish bool) *webrtc.API {
	t.Helper()
	o.Loopback = true
	api, err := sfuclient.NewAPI(o, publish)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

// negotiate has offerer offer and answerer answer, every candidate in the
// SDP (no trickling).
func negotiate(t *testing.T, offerer, answerer *webrtc.PeerConnection) {
	t.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered = webrtc.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}

// connected waits until every pc's connection is up.
func connected(t *testing.T, pcs ...*webrtc.PeerConnection) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, pc := range pcs {
		for pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			if time.Now().After(deadline) {
				t.Fatalf("not connected: %s", pc.ConnectionState())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// dropFirst drops the first sending of every video packet whose sequence
// number is 3 mod 7, and lets a resend of it through.
type dropFirst struct {
	interceptor.NoOp
	mu      sync.Mutex
	seen    map[uint32]map[uint16]bool
	dropped map[uint32]int
}

func newDropFirst() *dropFirst {
	return &dropFirst{seen: map[uint32]map[uint16]bool{}, dropped: map[uint32]int{}}
}

func (d *dropFirst) NewInterceptor(string) (interceptor.Interceptor, error) { return d, nil }

func (d *dropFirst) BindLocalStream(info *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	if !strings.HasPrefix(info.MimeType, "video/") {
		return w
	}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		d.mu.Lock()
		s := d.seen[h.SSRC]
		if s == nil {
			s = map[uint16]bool{}
			d.seen[h.SSRC] = s
		}
		drop := !s[h.SequenceNumber] && h.SequenceNumber%7 == 3
		s[h.SequenceNumber] = true
		if drop {
			d.dropped[h.SSRC]++
		}
		d.mu.Unlock()
		if drop {
			return len(payload), nil
		}
		return w.Write(h, payload, a)
	})
}

// layerRecv is what the SFU's side heard of one simulcast layer.
type layerRecv struct {
	id      string
	ssrc    webrtc.SSRC
	ref     uint16
	lo, hi  int // sequence numbers heard, relative to ref
	seqs    map[int]bool
	first   time.Time
	srs     int
	ridLast bool // the last packet still carried its rid
}

// recvLayers reads every track the SFU's connection gets, each layer's RTP
// and RTCP, into the map under mu.
func recvLayers(pc *webrtc.PeerConnection, mu *sync.Mutex, got map[string]*layerRecv, added chan<- string) {
	pc.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		var ridID uint8
		for _, e := range rcv.GetParameters().HeaderExtensions {
			if e.URI == sdp.SDESRTPStreamIDURI {
				ridID = uint8(e.ID)
			}
		}
		key := tr.ID() + "/" + tr.RID()
		l := &layerRecv{id: tr.ID(), ssrc: tr.SSRC(), seqs: map[int]bool{}}
		mu.Lock()
		got[key] = l
		mu.Unlock()
		added <- key
		go func() {
			for {
				p, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				mu.Lock()
				if l.first.IsZero() {
					l.first, l.ref = time.Now(), p.SequenceNumber
				}
				d := int(int16(p.SequenceNumber - l.ref))
				l.seqs[d] = true
				l.lo, l.hi = min(l.lo, d), max(l.hi, d)
				l.ridLast = ridID != 0 && len(p.GetExtension(ridID)) > 0
				mu.Unlock()
			}
		}()
		go func() {
			for {
				pkts, _, err := rcv.ReadSimulcastRTCP(tr.RID())
				if err != nil {
					return
				}
				for _, p := range pkts {
					if sr, ok := p.(*rtcp.SenderReport); ok && webrtc.SSRC(sr.SSRC) == l.ssrc {
						mu.Lock()
						l.srs++
						mu.Unlock()
					}
				}
			}
		}()
	})
}

// publishLayers adds a simulcast picture to pc: one sfuclient.Track per
// rid, lowest first, on one transceiver; PLIs each layer hears are counted
// in plis under mu.
func publishLayers(t *testing.T, pc *webrtc.PeerConnection, id string, rids []string, mu *sync.Mutex, plis map[string]int) map[string]*sfuclient.Track {
	t.Helper()
	layers := map[string]*sfuclient.Track{}
	var tx *webrtc.RTPTransceiver
	for _, rid := range rids {
		key := id + "/" + rid
		tr, err := sfuclient.NewTrack(codecs.H264, id, "alice", rid, func(p rtcp.Packet) {
			if _, ok := p.(*rtcp.PictureLossIndication); ok {
				mu.Lock()
				plis[key]++
				mu.Unlock()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		layers[rid] = tr
		if tx == nil {
			tx, err = pc.AddTransceiverFromTrack(tr, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		} else {
			err = tx.Sender().AddEncoding(tr)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range layers {
		tr.SetTransceiver(tx)
	}
	return layers
}

// frame is a stand-in access unit: one 3000-byte NAL unit, sent in three
// packets.
var frame = func() []byte {
	b := make([]byte, 3000)
	b[0] = 0x65
	return b
}()

// checkLayer fails unless the layer was heard, every packet between its
// first and last arriving (whatever was dropped was resent).
func checkLayer(t *testing.T, key string, l *layerRecv) {
	t.Helper()
	if l == nil || l.first.IsZero() {
		t.Errorf("%s: never heard", key)
		return
	}
	if n, want := len(l.seqs), l.hi-l.lo+1; n != want || n < 30 {
		t.Errorf("%s: %d packets of %d heard between the first and the last", key, n, want)
	}
}

func TestSimulcastByRID(t *testing.T) {
	n := testNet(t)
	drop := newDropFirst()
	cli := testPC(t, clientAPI(t, sfuclient.Options{Publish: []interceptor.Factory{drop}}, true))
	srv := testPC(t, sfuAPI(t, n, up))

	var mu sync.Mutex
	plis := map[string]int{}
	got := map[string]*layerRecv{}
	added := make(chan string, 16)
	recvLayers(srv, &mu, got, added)
	cam := publishLayers(t, cli, "cam", []string{"q", "h", "f"}, &mu, plis)
	negotiate(t, cli, srv)
	connected(t, cli, srv)

	// 30 frames a second on q and h from the start, on f from 1.5 s; a
	// PLI for h at 3 s.
	start := time.Now()
	ts, pli := uint32(0), false
	for el := time.Duration(0); el < 3500*time.Millisecond; el = time.Since(start) {
		for _, rid := range []string{"q", "h", "f"} {
			if rid == "f" && el < 1500*time.Millisecond {
				continue
			}
			if err := cam[rid].WriteFrame(frame, ts, nil); err != nil {
				t.Fatal(err)
			}
		}
		ts += 3000
		if el > 3*time.Second && !pli {
			pli = true
			mu.Lock()
			h := got["cam/h"]
			mu.Unlock()
			if h == nil {
				t.Fatal("no h layer by 3 s")
			}
			if err := srv.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(h.ssrc)}}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(33 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Errorf("tracks %v, want cam/q, cam/h, cam/f", keys(got))
	}
	for _, rid := range []string{"q", "h", "f"} {
		key := "cam/" + rid
		l := got[key]
		checkLayer(t, key, l)
		if l == nil {
			continue
		}
		if d := drop.dropped[uint32(l.ssrc)]; d == 0 {
			t.Errorf("%s: nothing dropped", key)
		} else {
			t.Logf("%s: %d packets heard, %d dropped once and resent; %d sender reports; first at %v", key, len(l.seqs), d, l.srs, l.first.Sub(start).Round(time.Millisecond))
		}
		if l.srs == 0 {
			t.Errorf("%s: no sender reports", key)
		}
		if l.ridLast {
			t.Errorf("%s: still writing its rid after receiver reports", key)
		}
	}
	if f := got["cam/f"]; f != nil && f.first.Sub(start) < 1500*time.Millisecond {
		t.Errorf("f heard at %v, before it started", f.first.Sub(start))
	}
	if plis["cam/h"] != 1 || plis["cam/q"] != 0 || plis["cam/f"] != 0 {
		t.Errorf("PLIs heard %v, want cam/h once", plis)
	}
}

func TestScreenBesideCamera(t *testing.T) {
	n := testNet(t)
	cli := testPC(t, clientAPI(t, sfuclient.Options{}, true))
	srv := testPC(t, sfuAPI(t, n, up))

	var mu sync.Mutex
	plis := map[string]int{}
	got := map[string]*layerRecv{}
	added := make(chan string, 16)
	recvLayers(srv, &mu, got, added)
	cam := publishLayers(t, cli, "cam", []string{"q", "h", "f"}, &mu, plis)
	negotiate(t, cli, srv)
	connected(t, cli, srv)

	var screen map[string]*sfuclient.Track
	start := time.Now()
	ts := uint32(0)
	for el := time.Duration(0); el < 3*time.Second; el = time.Since(start) {
		for _, tr := range cam {
			if err := tr.WriteFrame(frame, ts, nil); err != nil {
				t.Fatal(err)
			}
		}
		for _, tr := range screen {
			if err := tr.WriteFrame(frame, ts, nil); err != nil && err != sfuclient.ErrNotBound {
				t.Fatal(err)
			}
		}
		ts += 3000
		if el > time.Second && screen == nil {
			screen = publishLayers(t, cli, "screen", []string{"q", "f"}, &mu, plis)
			negotiate(t, cli, srv)
		}
		time.Sleep(33 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 5 {
		t.Errorf("tracks %v, want the camera's three layers and the screen's two", keys(got))
	}
	for _, key := range []string{"cam/q", "cam/h", "cam/f", "screen/q", "screen/f"} {
		checkLayer(t, key, got[key])
	}
}

func TestOnePortAndEarlyTracks(t *testing.T) {
	n := testNet(t)
	upAPI, downAPI := sfuAPI(t, n, up), sfuAPI(t, n, down)
	pubAPI, subAPI := clientAPI(t, sfuclient.Options{}, true), clientAPI(t, sfuclient.Options{}, false)

	// Two people, each with a connection to publish on and one to receive
	// on, and the SFU's side of all four on its one port. Each person's
	// voice is forwarded to the other, but only once the other's client
	// has announced the track: so that has to happen before any packet.
	type person struct {
		pub, sub, up, down *webrtc.PeerConnection
		mic                *sfuclient.Track
		fwd                *webrtc.TrackLocalStaticRTP
		announced          chan struct{}
		mu                 sync.Mutex
		heard              int
	}
	people := []*person{{}, {}}
	for i, p := range people {
		p.pub, p.sub = testPC(t, pubAPI), testPC(t, subAPI)
		p.up, p.down = testPC(t, upAPI), testPC(t, downAPI)
		p.announced = make(chan struct{})
		var err error
		if p.mic, err = sfuclient.NewTrack(codecs.Opus, "mic", []string{"alice", "bob"}[i], "", nil); err != nil {
			t.Fatal(err)
		}
		tx, err := p.pub.AddTransceiverFromTrack(p.mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			t.Fatal(err)
		}
		p.mic.SetTransceiver(tx)
		if p.fwd, err = webrtc.NewTrackLocalStaticRTP(codecs.Opus, "mic", []string{"bob", "alice"}[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := p.down.AddTransceiverFromTrack(p.fwd, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			t.Fatal(err)
		}
		p.sub.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			close(p.announced)
			for {
				if _, _, err := tr.ReadRTP(); err != nil {
					return
				}
				p.mu.Lock()
				p.heard++
				p.mu.Unlock()
			}
		})
	}
	for i, p := range people {
		other := people[1-i]
		p.up.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			select {
			case <-other.announced:
			case <-time.After(3 * time.Second):
				t.Errorf("person %d's track wasn't announced before its first packet", 1-i)
				return
			}
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				pkt.Extension, pkt.Extensions = false, nil
				if err := other.fwd.WriteRTP(pkt); err != nil {
					return
				}
			}
		})
	}
	for _, p := range people {
		negotiate(t, p.pub, p.up)
		negotiate(t, p.down, p.sub)
	}
	for _, p := range people {
		connected(t, p.pub, p.up, p.down, p.sub)
	}

	// A second of voice from each: 50 frames.
	start := time.Now()
	lvl := uint8(30)
	for i := 0; i < 50; i++ {
		for _, p := range people {
			if err := p.mic.WriteFrame([]byte{0xf8, 0xff, 0xfe}, uint32(i*960), &lvl); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * 20 * time.Millisecond)))
	}
	time.Sleep(200 * time.Millisecond)

	port := n.addrs()[0].(*net.UDPAddr).Port
	for i, p := range people {
		p.mu.Lock()
		heard := p.heard
		p.mu.Unlock()
		if heard < 45 {
			t.Errorf("person %d heard %d of the other's 50 frames", i, heard)
		}
		for _, pc := range []*webrtc.PeerConnection{p.up, p.down} {
			pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil || pair == nil {
				t.Fatalf("person %d: no candidate pair: %v", i, err)
			}
			if int(pair.Local.Port) != port {
				t.Errorf("person %d: the SFU's side on port %d, not its one port %d", i, pair.Local.Port, port)
			}
		}
	}
}

func keys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
