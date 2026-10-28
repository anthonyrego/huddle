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

// Adapted from livekit/server-sdk-go v2.19.0 transport.go; changed: the
// SFU's two roles (answering on up, offering on down), the down tracks put
// on the connection just before each offer, no ICE restarts, a batching
// window in place of the debounce.

package sfu

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// negotiateAfter gathers the changes to a down connection (tracks coming
// and going, a joiner's whole room at once) into one offer.
const negotiateAfter = 20 * time.Millisecond

// transport is one of a participant's two connections. On up the client
// offers and the SFU answers. On down the SFU offers: the down tracks the
// room says the participant should have are put on the connection, then
// offered; with an offer out, a change waits for the answer and is offered
// after it.
type transport struct {
	p    *participant
	role role
	pc   *webrtc.PeerConnection
	name signal.PC

	// op serializes what changes the connection's descriptions, and holds
	// the client's candidates until there's a remote description to add
	// them to.
	op      sync.Mutex
	pending []webrtc.ICECandidateInit
	senders map[string]*webrtc.RTPSender // down: each down track's, by SID

	mu       sync.Mutex
	offering bool        // down: an offer is out and its answer isn't in
	again    bool        // down: something changed since; offer after the answer
	timer    *time.Timer // down: the batching window
	closed   bool
}

func newTransport(p *participant, r role) (*transport, error) {
	api, name := p.s.upAPI, signal.Pub
	if r == down {
		api, name = p.s.downAPI, signal.Sub
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r, err)
	}
	t := &transport{p: p, role: r, pc: pc, name: name, senders: map[string]*webrtc.RTPSender{}}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		p.send(signal.KindICE, signal.ICE{PC: name, Candidate: j.Candidate, SDPMid: j.SDPMid, SDPMLineIndex: j.SDPMLineIndex, UsernameFragment: j.UsernameFragment})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		p.log.Debug("sfu: connection", "pc", r.String(), "state", st.String())
		if st == webrtc.PeerConnectionStateFailed {
			p.log.Warn("sfu: connection failed", "pc", r.String())
			p.kick("the " + string(name) + " connection failed")
		}
	})
	if r == up {
		pc.OnTrack(p.onTrack)
	}
	return t, nil
}

// connected says media can flow (down tracks write nothing until then).
// It asks pion, whose state-change handlers run on goroutines of their own
// and can come in either order.
func (t *transport) connected() bool {
	return t.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}

// answer takes the client's offer on up and sends the answer.
func (t *transport) answer(sdp string) error {
	t.op.Lock()
	defer t.op.Unlock()
	if err := t.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return err
	}
	t.addPending()
	ans, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := t.pc.SetLocalDescription(ans); err != nil {
		return err
	}
	t.p.send(signal.KindAnswer, signal.SDP{PC: t.name, SDP: ans.SDP})
	return nil
}

// addCandidate adds one of the client's candidates, or holds it until the
// remote description is set.
func (t *transport) addCandidate(c webrtc.ICECandidateInit) error {
	t.op.Lock()
	defer t.op.Unlock()
	if t.pc.RemoteDescription() == nil {
		t.pending = append(t.pending, c)
		return nil
	}
	return t.pc.AddICECandidate(c)
}

// addPending adds the candidates held for a remote description; op is held.
func (t *transport) addPending() {
	for _, c := range t.pending {
		if err := t.pc.AddICECandidate(c); err != nil {
			t.p.log.Info("sfu: a candidate", "pc", t.role.String(), "err", err)
		}
	}
	t.pending = nil
}

// negotiate offers down again soon, with whatever has changed by then.
func (t *transport) negotiate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.timer != nil {
		return
	}
	t.timer = time.AfterFunc(negotiateAfter, t.offer)
}

// offer puts the room's down tracks for the participant on the connection
// and offers them, unless an offer is already out (then it's done after
// the answer).
func (t *transport) offer() {
	t.mu.Lock()
	t.timer = nil
	if t.closed {
		t.mu.Unlock()
		return
	}
	if t.offering {
		t.again = true
		t.mu.Unlock()
		return
	}
	t.offering = true
	t.mu.Unlock()

	want := t.p.rm.Load().downsOf(t.p)
	t.op.Lock()
	sdp, err := t.offerLocked(want)
	t.op.Unlock()
	if err != nil {
		t.mu.Lock()
		t.offering = false
		closed := t.closed
		t.mu.Unlock()
		if !closed {
			t.p.log.Error("sfu: an offer", "err", err)
		}
		return
	}
	if sdp == "" {
		return
	}
	t.p.send(signal.KindOffer, signal.SDP{PC: t.name, SDP: sdp})
}

// offerLocked makes the connection's senders match want and offers; op is
// held. Nothing to offer (no track ever) is an empty SDP.
func (t *transport) offerLocked(want map[string]*downTrack) (string, error) {
	for sid, s := range t.senders {
		if want[sid] == nil {
			if err := t.pc.RemoveTrack(s); err != nil && !errors.Is(err, webrtc.ErrConnectionClosed) {
				t.p.log.Info("sfu: removing a down track", "track", sid, "err", err)
			}
			delete(t.senders, sid)
		}
	}
	for sid, dt := range want {
		if t.senders[sid] != nil {
			continue
		}
		s, err := t.pc.AddTrack(dt)
		if err != nil {
			return "", fmt.Errorf("add %s: %w", sid, err)
		}
		t.senders[sid] = s
	}
	if len(t.pc.GetTransceivers()) == 0 {
		t.mu.Lock()
		t.offering = false
		t.mu.Unlock()
		return "", nil
	}
	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err := t.pc.SetLocalDescription(offer); err != nil {
		return "", err
	}
	return offer.SDP, nil
}

// setAnswer takes the client's answer on down, then offers again if
// anything changed while it was out.
func (t *transport) setAnswer(sdp string) error {
	t.op.Lock()
	if t.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		t.op.Unlock()
		return errors.New("no offer is out")
	}
	err := t.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
	if err == nil {
		t.addPending()
	}
	t.op.Unlock()
	t.mu.Lock()
	t.offering = false
	again := t.again && err == nil
	t.again = false
	t.mu.Unlock()
	if again {
		go t.offer()
	}
	return err
}

// close closes the connection.
func (t *transport) close() {
	t.mu.Lock()
	t.closed = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.mu.Unlock()
	if err := t.pc.Close(); err != nil {
		t.p.log.Debug("sfu: closing a connection", "pc", t.role.String(), "err", err)
	}
}
