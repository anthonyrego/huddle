package sfu

import (
	"fmt"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
)

// role is which way a connection's media goes, seen from the SFU. Each
// person has one of each, as with LiveKit: the client offers on the one it
// publishes on, the SFU on the one it sends on, so neither side's offers
// ever cross.
type role int

const (
	// up: a person publishes on it and the SFU receives.
	up role = iota
	// down: the SFU sends everyone else's tracks on it.
	down
)

func (r role) String() string {
	if r == up {
		return "up"
	}
	return "down"
}

// NACKs for a packet missing upstream: every nackEvery while it's still
// missing, at most nackTimes, which covers the 250 ms a receiver's
// assembler waits for it.
const (
	nackEvery = 20 * time.Millisecond
	nackTimes = 10
)

// nackKept is how many packets a down track keeps to resend: about 2 s of a
// 1080p screen.
const nackKept = 512

// newAPI makes the pion API for one role, with extra interceptors nearest
// the network (congestion control; a narrow link in tests). Up: pion's NACK
// generator (the publisher resends what didn't arrive), receiver reports,
// and TWCC feedback for the publisher's congestion control. Down:
// transport-wide sequence numbers, then pion's NACK responder (a viewer's
// NACK is answered from what the down track sent) outside them, so a
// resend gets a number of its own as congestion control needs. Down tracks
// write their own sender reports, translated from the publisher's, so not
// pion's (it would map RTP time to when the SFU forwarded each packet).
func newAPI(n *network, r role, extra ...interceptor.Factory) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := codecs.Register(m); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	for _, f := range extra {
		ir.Add(f)
	}
	switch r {
	case up:
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
	case down:
		seq, err := twcc.NewHeaderExtensionInterceptor()
		if err != nil {
			return nil, err
		}
		resp, err := nack.NewResponderInterceptor(nack.ResponderSize(nackKept))
		if err != nil {
			return nil, err
		}
		ir.Add(seq)
		ir.Add(resp)
	}
	s, err := n.settings()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r, err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(s)), nil
}
