// Package codecs is what huddle's media connections speak, on both ends (the
// clients and the SFU): Opus and H.264 Constrained Baseline, the RTCP
// feedback each takes, and the RTP header extensions they read. Each end adds
// the interceptors that act on the feedback itself (pion's Configure helpers
// would register it a second time).
//
// There's no RTX: pion's NACK responder makes RTX packets by copying the
// original header and never writes repaired-rid, so the SFU couldn't tell
// which simulcast layer an RTX stream repairs. Without RTX, a retransmission
// goes out on the stream itself, and pion's NACK generator counts it as the
// packet arriving late. Pure.
package codecs

import (
	"fmt"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// Payload types: Opus at the number every WebRTC stack uses, H.264 at
// pion's for Constrained Baseline with packetization mode 1.
const (
	OpusPT = 111
	H264PT = 102
)

// Opus is the voice: 48 kHz, 20 ms frames, in-band FEC and DTX. Channels is
// 2 as WebRTC requires; what's sent is mono. Its loss is covered by FEC and
// concealment, not retransmission, so it takes no nack.
var Opus = webrtc.RTPCodecCapability{
	MimeType:     webrtc.MimeTypeOpus,
	ClockRate:    48000,
	Channels:     2,
	SDPFmtpLine:  "minptime=10;useinbandfec=1;usedtx=1",
	RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}},
}

// H264 is video: Constrained Baseline, what VideoToolbox's real-time
// encoder makes (macvideo.Encoder).
var H264 = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	},
}

// Register puts the codecs and header extensions on m: transport-wide
// sequence numbers on everything (congestion control), the RFC 6464 audio
// level on audio (the SFU's speaker detection reads it), and mid and rid on
// video (a simulcast layer's SSRC isn't in the SDP; the receiver learns it
// from these on the first packets).
func Register(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: Opus, PayloadType: OpusPT}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("register opus: %w", err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: H264, PayloadType: H264PT}, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("register h264: %w", err)
	}
	exts := []struct {
		uri  string
		kind webrtc.RTPCodecType
	}{
		{sdp.TransportCCURI, webrtc.RTPCodecTypeAudio},
		{sdp.TransportCCURI, webrtc.RTPCodecTypeVideo},
		{sdp.AudioLevelURI, webrtc.RTPCodecTypeAudio},
		{sdp.SDESMidURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
	}
	for _, e := range exts {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: e.uri}, e.kind); err != nil {
			return fmt.Errorf("register %s: %w", e.uri, err)
		}
	}
	return nil
}
