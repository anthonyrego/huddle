package sfu

// H.264 NAL unit types the forwarder looks for in RTP payloads (RFC 6184).
const (
	nalSEI   = 6
	nalSPS   = 7
	nalSTAPA = 24
	nalFUA   = 28
)

// seiFiller is the SEI message a publisher's probe fills what its encoder
// left unused with (internal/rtc/room's video.go): it's for the
// publisher's uplink, and decoders ignore it.
const seiFiller = 3

// fillerStub is what a viewer is sent in place of a packet of filler: a
// padding-only packet's payload, one byte of padding (its length), so the
// packet keeps its sequence number and marker and costs a header.
var fillerStub = []byte{1}

// filler says whether an H.264 RTP payload carries a publisher's probe
// filler and nothing else: a single SEI NAL unit whose first message is a
// filler payload, or a fragment of one. inFU carries, between packets,
// whether the FU-A being read is one, from its first fragment to its last.
func filler(payload []byte, inFU *bool) bool {
	if len(payload) < 2 {
		return false
	}
	switch payload[0] & 0x1f {
	case nalSEI:
		return payload[1] == seiFiller
	case nalFUA:
		if payload[1]&0x1f != nalSEI {
			*inFU = false
			return false
		}
		if payload[1]&0x80 != 0 { // its first fragment
			*inFU = len(payload) > 2 && payload[2] == seiFiller
		}
		f := *inFU
		if payload[1]&0x40 != 0 { // its last
			*inFU = false
		}
		return f
	}
	return false
}

// keyframeStart says whether an H.264 RTP payload is the first packet of a
// keyframe: it carries the SPS, which our encoder puts first in every
// keyframe (macvideo.Encoder: SPS and PPS ahead of each IDR). The payload
// is a single NAL unit, a STAP-A (pion's payloader puts the SPS and PPS in
// one), or the first fragment of an FU-A.
func keyframeStart(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	switch t := payload[0] & 0x1f; {
	case t == nalSTAPA:
		for i := 1; i+2 < len(payload); {
			n := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if n == 0 || i+n > len(payload) {
				return false
			}
			if payload[i]&0x1f == nalSPS {
				return true
			}
			i += n
		}
		return false
	case t == nalFUA:
		return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&0x1f == nalSPS
	default:
		return t == nalSPS
	}
}
