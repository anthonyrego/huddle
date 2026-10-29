package sfuclient

// What a session asks of the tracks it's sent: a video's size (the server
// picks the layer that fits it), or none; and a keyframe.

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Settings tells the server how this client wants a video track it's sent:
// the size it's shown at, in pixels, or off (Enabled false, or no size:
// nothing is sent). The server sends a video track nothing until its first
// settings, or a quarter of a second after the track is on the receive
// connection (then the largest layer), as signal.Settings says.
func (s *Session) Settings(st signal.Settings) error {
	return s.send(signal.KindSettings, st)
}

// PLI asks the publisher of a track this client is sent for a keyframe: a
// picture loss indication for the track's SSRC (its RemoteTrack's
// Track.SSRC()), on the receive connection.
func (s *Session) PLI(ssrc webrtc.SSRC) error {
	return s.sub.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{SenderSSRC: uint32(ssrc), MediaSSRC: uint32(ssrc)}})
}
