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

// Adapted from livekit/server-sdk-go v2.19.0 localtrack.go; changed: frames
// in at the caller's RTP timestamps (no samples, durations or providers),
// no E2EE or abs-capture-time, RTCP handed over a packet at a time.

package sfuclient

import (
	"errors"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// mtu is the most an RTP packet we send carries, header included.
const mtu = 1200

// rtpHeaderRoom is what a packet's header and extensions may take of the
// mtu: 12 bytes of header, then the extensions (audio level or mid and rid,
// and the transport-wide sequence number).
const rtpHeaderRoom = 48

// ErrNotBound is a write to a track that isn't negotiated yet (or any
// more); the frame is dropped.
var ErrNotBound = errors.New("track not bound")

// Track is one stream we send: a mic, or one simulcast layer of a camera or
// screen. Until a receiver report names its SSRC it writes mid and rid on
// every packet (pion reads them, but never writes them), so the SFU can
// tell the layers apart; and on audio it writes the RFC 6464 level, which
// the SFU's speaker detection reads.
type Track struct {
	local  *webrtc.TrackLocalStaticRTP
	rid    string
	onRTCP func(rtcp.Packet)

	mu        sync.Mutex
	tr        *webrtc.RTPTransceiver
	bound     bool
	ssrc      webrtc.SSRC
	acked     bool
	levelID   uint8
	midID     uint8
	ridID     uint8
	payloader rtp.Payloader
	seq       rtp.Sequencer
}

// NewTrack makes a track of codec c. Simulcast layers of one picture share
// id and streamID and differ in rid; a mic has no rid. onRTCP, when set, is
// told of every RTCP packet the receiver sends about this track (a PLI or
// FIR asks this layer's encoder for a keyframe).
func NewTrack(c webrtc.RTPCodecCapability, id, streamID, rid string, onRTCP func(rtcp.Packet)) (*Track, error) {
	var opts []func(*webrtc.TrackLocalStaticRTP)
	if rid != "" {
		opts = append(opts, webrtc.WithRTPStreamID(rid))
	}
	local, err := webrtc.NewTrackLocalStaticRTP(c, id, streamID, opts...)
	if err != nil {
		return nil, err
	}
	return &Track{local: local, rid: rid, onRTCP: onRTCP}, nil
}

// ID, RID, StreamID, Kind and Codec are webrtc.TrackLocal's.
func (t *Track) ID() string                       { return t.local.ID() }
func (t *Track) RID() string                      { return t.rid }
func (t *Track) StreamID() string                 { return t.local.StreamID() }
func (t *Track) Kind() webrtc.RTPCodecType        { return t.local.Kind() }
func (t *Track) Codec() webrtc.RTPCodecCapability { return t.local.Codec() }

// SetTransceiver names the transceiver that sends the track, whose mid goes
// on the packets.
func (t *Track) SetTransceiver(tr *webrtc.RTPTransceiver) {
	t.mu.Lock()
	t.tr = tr
	t.mu.Unlock()
}

// SSRC is the stream's SSRC once bound.
func (t *Track) SSRC() webrtc.SSRC {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ssrc
}

// Bind is webrtc.TrackLocal's, called when the track is negotiated.
func (t *Track) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := t.local.Bind(ctx)
	if err != nil {
		return codec, err
	}
	var pay rtp.Payloader = &codecs.OpusPayloader{}
	if codec.MimeType == webrtc.MimeTypeH264 {
		pay = &codecs.H264Payloader{}
	}
	t.mu.Lock()
	t.bound, t.acked, t.ssrc = true, false, ctx.SSRC()
	t.levelID, t.midID, t.ridID = 0, 0, 0
	for _, e := range ctx.HeaderExtensions() {
		switch e.URI {
		case sdp.AudioLevelURI:
			t.levelID = uint8(e.ID)
		case sdp.SDESMidURI:
			t.midID = uint8(e.ID)
		case sdp.SDESRTPStreamIDURI:
			t.ridID = uint8(e.ID)
		}
	}
	t.payloader, t.seq = pay, rtp.NewRandomSequencer()
	t.mu.Unlock()
	go t.readRTCP(ctx.RTCPReader(), ctx.SSRC())
	return codec, nil
}

// Unbind is webrtc.TrackLocal's.
func (t *Track) Unbind(ctx webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.bound = false
	t.mu.Unlock()
	return t.local.Unbind(ctx)
}

// readRTCP reads what the receiver says about this stream until the
// binding ends (pion's interceptors need it read).
func (t *Track) readRTCP(r interceptor.RTCPReader, ssrc webrtc.SSRC) {
	b := make([]byte, 1500)
	for {
		n, _, err := r.Read(b, interceptor.Attributes{})
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(b[:n])
		if err != nil {
			continue
		}
		for _, p := range pkts {
			if rr, ok := p.(*rtcp.ReceiverReport); ok {
				for _, r := range rr.Reports {
					if webrtc.SSRC(r.SSRC) == ssrc {
						t.mu.Lock()
						t.acked = true
						t.mu.Unlock()
					}
				}
			}
			if t.onRTCP != nil {
				t.onRTCP(p)
			}
		}
	}
}

// WriteFrame sends one frame (an Opus packet, or an H.264 access unit in
// Annex-B) at RTP timestamp ts, split into packets of at most mtu, the
// marker on the last. level, when set, is the frame's RFC 6464 level
// (-dBov, 0–127), written on audio.
func (t *Track) WriteFrame(frame []byte, ts uint32, level *uint8) error {
	t.mu.Lock()
	if !t.bound {
		t.mu.Unlock()
		return ErrNotBound
	}
	pay, seq := t.payloader, t.seq
	levelID, midID, ridID := t.levelID, t.midID, t.ridID
	var mid string
	if t.rid != "" && !t.acked && t.tr != nil {
		mid = t.tr.Mid()
	}
	t.mu.Unlock()

	payloads := pay.Payload(mtu-rtpHeaderRoom, frame)
	for i, p := range payloads {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         i == len(payloads)-1,
				SequenceNumber: seq.NextSequenceNumber(),
				Timestamp:      ts,
			},
			Payload: p,
		}
		if level != nil && levelID != 0 {
			b, err := rtp.AudioLevelExtension{Level: *level}.Marshal()
			if err != nil {
				return err
			}
			if err := pkt.SetExtension(levelID, b); err != nil {
				return err
			}
		}
		if mid != "" && midID != 0 && ridID != 0 {
			if err := pkt.SetExtension(midID, []byte(mid)); err != nil {
				return err
			}
			if err := pkt.SetExtension(ridID, []byte(t.rid)); err != nil {
				return err
			}
		}
		if err := t.local.WriteRTP(pkt); err != nil {
			return err
		}
	}
	return nil
}
