// Package sfuclient joins huddle's own SFU (internal/server/sfu): signalling
// on the server's /rtc WebSocket, one connection to publish on and one to
// receive on, and tracks that write what the SFU reads. It replaces
// LiveKit's Go SDK, adapted from it where noted (Apache 2.0; see NOTICE).
// The room package builds the app's voice and video on it; the bots use it
// directly. Pure.
package sfuclient

import (
	"log/slog"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
)

// Connection settings (livekit/server-sdk-go v2.19.0 transport.go's
// values; the SFU uses the same).
const (
	dtlsRetransmission = 100 * time.Millisecond
	iceDisconnected    = 10 * time.Second
	iceFailed          = 5 * time.Second
	iceKeepalive       = 2 * time.Second
)

// NACKs for a packet missing on the way in: every nackEvery while it's
// still missing, at most nackTimes (the assembler waits 250 ms for it).
// nackKept is how many sent packets are kept to resend.
const (
	nackEvery = 20 * time.Millisecond
	nackTimes = 10
	nackKept  = 512
)

// Options tune a session's connections.
type Options struct {
	// Publish and Subscribe are interceptors put on each connection ahead
	// of the session's own, nearest the network: the uplink limit and
	// congestion control on the one we publish on, the network simulation
	// on the one we receive on. A session makes its publish connection
	// once, inside Join, and the Publish factories go on that connection
	// alone: a cc.InterceptorFactory made for the session hands its
	// OnNewPeerConnection the publish connection's estimator, once, before
	// Join returns.
	Publish   []interceptor.Factory
	Subscribe []interceptor.Factory
	// Loopback offers 127.0.0.1 as a candidate (tests).
	Loopback bool
	// SendOnly subscribes to nothing (bots that only publish); the server
	// still says who's there and what they publish.
	SendOnly bool
	// Log takes the session's lines (sfuclient: …); nil is slog.Default().
	Log *slog.Logger
}

// NewAPI makes the pion API for one of a session's connections (the session
// uses it; so do tests that drive pion directly). Publish: the caller's
// interceptors, then transport-wide sequence numbers (congestion control
// among the caller's reads them, and the SFU's TWCC feedback answers
// them), then pion's NACK responder outside them (so a resend gets a
// number of its own) and sender reports (outermost, so they see packets as
// they're written, before any pacing: the reports map RTP time to the
// capture clock). A caller passes congestion control and its pacer, never
// the transport-wide numbers again. Receive: the caller's, then pion's NACK
// generator, receiver reports, and TWCC feedback for the SFU's congestion
// control.
func NewAPI(o Options, publish bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := codecs.Register(m); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if publish {
		for _, f := range o.Publish {
			ir.Add(f)
		}
		seq, err := twcc.NewHeaderExtensionInterceptor()
		if err != nil {
			return nil, err
		}
		resp, err := nack.NewResponderInterceptor(nack.ResponderSize(nackKept))
		if err != nil {
			return nil, err
		}
		sr, err := report.NewSenderInterceptor()
		if err != nil {
			return nil, err
		}
		ir.Add(seq)
		ir.Add(resp)
		ir.Add(sr)
	} else {
		for _, f := range o.Subscribe {
			ir.Add(f)
		}
		gen, err := nack.NewGeneratorInterceptor(nack.GeneratorInterval(nackEvery), nack.GeneratorMaxNacksPerPacket(nackTimes))
		if err != nil {
			return nil, err
		}
		rr, err := report.NewReceiverInterceptor()
		if err != nil {
			return nil, err
		}
		fb, err := twcc.NewSenderInterceptor()
		if err != nil {
			return nil, err
		}
		ir.Add(gen)
		ir.Add(rr)
		ir.Add(fb)
	}
	var s webrtc.SettingEngine
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	s.SetIncludeLoopbackCandidate(o.Loopback)
	s.SetSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80)
	s.SetDTLSRetransmissionInterval(dtlsRetransmission)
	s.SetICETimeouts(iceDisconnected, iceFailed, iceKeepalive)
	if !publish {
		// A remote track exists as soon as the SDP names it, so a track
		// that hasn't sent yet is still announced (and nothing pads it).
		s.SetFireOnTrackBeforeFirstRTP(true)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(s)), nil
}
