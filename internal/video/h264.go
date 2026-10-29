package video

import (
	"encoding/binary"
	"errors"
)

// H.264's two byte formats. The network (RTP, and pion's packetizer) and
// the app's own frames use Annex-B, NAL units each after a start code;
// VideoToolbox uses AVCC, each after its length in 4 bytes, with the
// parameter sets (SPS and PPS) kept apart in a format description.

// NAL unit types the app looks at.
const (
	NALSlice = 1
	NALIDR   = 5 // a keyframe's slice
	NALSEI   = 6
	NALSPS   = 7
	NALPPS   = 8
	NALAUD   = 9
)

// NALType is a NAL unit's type.
func NALType(nal []byte) int {
	if len(nal) == 0 {
		return 0
	}
	return int(nal[0] & 0x1f)
}

// SplitAnnexB returns the NAL units of an Annex-B access unit, start codes
// removed (3- or 4-byte), sharing b's memory.
func SplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && b[end-1] == 0 {
					end-- // a 4-byte start code's leading zero
				}
				out = append(out, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

var startCode = []byte{0, 0, 0, 1}

// AppendAnnexB appends NAL units, each after a 4-byte start code.
func AppendAnnexB(dst []byte, nals ...[]byte) []byte {
	for _, n := range nals {
		dst = append(append(dst, startCode...), n...)
	}
	return dst
}

// AppendAVCC appends NAL units, each after its length in 4 bytes.
func AppendAVCC(dst []byte, nals ...[]byte) []byte {
	for _, n := range nals {
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(n)))
		dst = append(dst, n...)
	}
	return dst
}

// AVCCToAnnexB appends the NAL units of AVCC data to dst as Annex-B.
func AVCCToAnnexB(dst, avcc []byte) ([]byte, error) {
	for len(avcc) > 0 {
		if len(avcc) < 4 {
			return dst, errors.New("avcc: a length cut short")
		}
		n := int(binary.BigEndian.Uint32(avcc))
		avcc = avcc[4:]
		if n > len(avcc) {
			return dst, errors.New("avcc: a NAL unit longer than what's left")
		}
		dst = AppendAnnexB(dst, avcc[:n])
		avcc = avcc[n:]
	}
	return dst, nil
}

// IsKeyframe reports whether an Annex-B access unit holds an IDR slice.
func IsKeyframe(au []byte) bool {
	for _, n := range SplitAnnexB(au) {
		if NALType(n) == NALIDR {
			return true
		}
	}
	return false
}
