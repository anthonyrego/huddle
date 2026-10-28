// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Adapted from livekit/server-sdk-go v2.19.0 transport.go; changed: one
// type for both of a session's connections (offering on Pub, answering on
// Sub), no ICE restarts, a batching window in place of the debounce.

package sfuclient

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// negotiateAfter gathers what changes on Pub (tracks published together)
// into one offer.
const negotiateAfter = 20 * time.Millisecond

// peer is one of a session's two connections. On Pub the client offers:
// with an offer out, a change waits for the answer and is offered after it.
// On Sub it answers the server's offers. Candidates trickle both ways; the
// server's are held until there's a remote description to add them to.
type peer struct {
	s    *Session
	name signal.PC
	pc   *webrtc.PeerConnection

	// op serializes what changes the descriptions, and holds the server's
	// early candidates.
	op      sync.Mutex
	pending []webrtc.ICECandidateInit

	mu       sync.Mutex
	offering bool        // Pub: an offer is out and its answer isn't in
	again    bool        // Pub: something changed since; offer after the answer
	timer    *time.Timer // Pub: the batching window
	closed   bool
}

func newPeer(s *Session, name signal.PC, api *webrtc.API) (*peer, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	p := &peer{s: s, name: name, pc: pc}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		s.send(signal.KindICE, signal.ICE{PC: name, Candidate: j.Candidate, SDPMid: j.SDPMid, SDPMLineIndex: j.SDPMLineIndex, UsernameFragment: j.UsernameFragment})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.log.Debug("sfuclient: connection", "pc", name, "state", st.String())
		s.push(func() {
			if s.ev.Connection != nil {
				s.ev.Connection(name, st)
			}
		})
		if st == webrtc.PeerConnectionStateFailed {
			s.fail(fmt.Errorf("the %s connection failed", name))
		}
	})
	return p, nil
}

// negotiate offers Pub again soon, with whatever has changed by then.
func (p *peer) negotiate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.timer != nil {
		return
	}
	p.timer = time.AfterFunc(negotiateAfter, p.offer)
}

// offer offers Pub, unless an offer is out already (then it's done after
// the answer).
func (p *peer) offer() {
	p.mu.Lock()
	p.timer = nil
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.offering {
		p.again = true
		p.mu.Unlock()
		return
	}
	p.offering = true
	p.mu.Unlock()

	p.op.Lock()
	sdp, err := p.offerLocked()
	p.op.Unlock()
	if err != nil {
		p.mu.Lock()
		p.offering = false
		closed := p.closed
		p.mu.Unlock()
		if !closed {
			p.s.log.Warn("sfuclient: an offer", "pc", p.name, "err", err)
		}
		return
	}
	p.s.send(signal.KindOffer, signal.SDP{PC: p.name, SDP: sdp})
}

func (p *peer) offerLocked() (string, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", err
	}
	return offer.SDP, nil
}

// setAnswer takes the server's answer on Pub, then offers again if
// anything changed while it was out.
func (p *peer) setAnswer(sdp string) error {
	p.op.Lock()
	if p.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		p.op.Unlock()
		return errors.New("no offer is out")
	}
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
	if err == nil {
		p.addPending()
	}
	p.op.Unlock()
	p.mu.Lock()
	p.offering = false
	again := p.again && err == nil
	p.again = false
	p.mu.Unlock()
	if again {
		go p.offer()
	}
	return err
}

// answer takes the server's offer on Sub and sends the answer.
func (p *peer) answer(sdp string) error {
	p.op.Lock()
	defer p.op.Unlock()
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return err
	}
	p.addPending()
	ans, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(ans); err != nil {
		return err
	}
	return p.s.send(signal.KindAnswer, signal.SDP{PC: p.name, SDP: ans.SDP})
}

// addCandidate adds one of the server's candidates, or holds it until the
// remote description is set.
func (p *peer) addCandidate(c webrtc.ICECandidateInit) error {
	p.op.Lock()
	defer p.op.Unlock()
	if p.pc.RemoteDescription() == nil {
		p.pending = append(p.pending, c)
		return nil
	}
	return p.pc.AddICECandidate(c)
}

// addPending adds the candidates held for a remote description; op is held.
func (p *peer) addPending() {
	for _, c := range p.pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			p.s.log.Debug("sfuclient: a candidate", "pc", p.name, "err", err)
		}
	}
	p.pending = nil
}

// close closes the connection.
func (p *peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
	if err := p.pc.Close(); err != nil {
		p.s.log.Debug("sfuclient: closing a connection", "pc", p.name, "err", err)
	}
}
