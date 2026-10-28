package sfu

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
)

// pion's GCC on the SFU's sending side, the viewer's TWCC feedback driving
// it, through a 1 Mbps link. The sender follows the estimate, as the down
// tracks' allocation will (90% of it, up to 2.5 Mbps): from 5 s on, what's
// sent stays near the link. Then, sending a steady 250 kbps, the estimate
// climbs anyway (logged: pion's GCC has no cap at what was acknowledged, so
// the allocation caps it, as the app's alloc.go does). A sender that
// ignores the estimate isn't a case: pion's pacers drop what overflows
// them after the transport-wide numbers are given, which reads as loss and
// pins the estimate at its floor.
func TestGCCOnDownlink(t *testing.T) {
	if testing.Short() {
		t.Skip("16 s of sending")
	}
	n := testNet(t)
	ests := make(chan cc.BandwidthEstimator, 1)
	ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(1_200_000), gcc.SendSideBWEMinBitrate(100_000), gcc.SendSideBWEMaxBitrate(8_000_000),
			gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
	})
	if err != nil {
		t.Fatal(err)
	}
	ccf.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { ests <- e })
	var link *icept.Link
	narrow := &icept.Uplink{BPS: 1_000_000, Slots: icept.UplinkSlots, Made: func(l *icept.Link) { link = l }}
	srv := testPC(t, sfuAPI(t, n, down, narrow, ccf))
	viewer := testPC(t, clientAPI(t, sfuclient.Options{}, false))

	track, err := webrtc.NewTrackLocalStaticRTP(codecs.H264, "cam", "alice")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := srv.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	// The viewer's feedback reaches the interceptors only as it's read (a
	// down track's Bind reads it).
	go func() {
		for {
			if _, _, err := tx.Sender().ReadRTCP(); err != nil {
				return
			}
		}
	}()
	var heard atomic.Int64
	viewer.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			heard.Add(int64(p.MarshalSize()))
		}
	})
	negotiate(t, srv, viewer)
	connected(t, srv, viewer)
	est := <-ests

	// send writes 1100-byte packets for d at the rate rate gives (asked
	// again every 100 ms, from the estimate), sampling what was sent and
	// the estimate every 500 ms.
	payload := make([]byte, 1100)
	payload[0] = 0x41
	var seq uint16
	send := func(d time.Duration, rate func(est int) int) (sentKbps, estKbps []int) {
		start := time.Now()
		next, ask, sample := start, start, start.Add(500*time.Millisecond)
		bps, bytes := rate(est.GetTargetBitrate()), 0
		for time.Since(start) < d {
			seq++
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(time.Since(start) / time.Millisecond * 90)}, Payload: payload}
			if err := track.WriteRTP(pkt); err != nil {
				t.Fatal(err)
			}
			bytes += pkt.MarshalSize()
			now := time.Now()
			if now.After(ask) {
				bps, ask = rate(est.GetTargetBitrate()), ask.Add(100*time.Millisecond)
			}
			if now.After(sample) {
				sentKbps = append(sentKbps, bytes*8*2/1000)
				estKbps = append(estKbps, est.GetTargetBitrate()/1000)
				bytes, sample = 0, sample.Add(500*time.Millisecond)
			}
			next = next.Add(time.Duration(float64(time.Second) * float64(pkt.MarshalSize()*8) / float64(bps)))
			time.Sleep(time.Until(next))
		}
		return sentKbps, estKbps
	}

	sent, ests1 := send(15*time.Second, func(e int) int { return min(e*9/10, 2_500_000) })
	t.Logf("following the estimate through 1 Mbps, every 500 ms: sent %v kbps, estimate %v kbps; the link dropped %d", sent, ests1, link.Dropped())
	// From 5 s on (sample 10): what's sent stays near the link.
	var sum int
	for i, k := range sent[10:] {
		sum += k
		if k < 500 || k > 1300 {
			t.Errorf("at %v: sent %d kbps", time.Duration(11+i)*500*time.Millisecond, k)
		}
	}
	if avg := sum / len(sent[10:]); avg < 750 || avg > 1150 {
		t.Errorf("sent %d kbps on average from 5 s on, through a 1 Mbps link", avg)
	}
	_, ests2 := send(6*time.Second, func(int) int { return 250_000 })
	t.Logf("then a steady 250 kbps: estimate %v kbps (it climbs: the allocation caps it at what was sent)", ests2)
}

func kbps(b []int) []int {
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = v / 1000
	}
	return out
}
