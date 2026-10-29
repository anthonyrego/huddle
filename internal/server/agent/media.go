package agent

import (
	"context"
	"errors"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Media is how the whiteboard reaches a room's media: huddled's own SFU,
// joined in-process (Builtin, sfuroom.go). One serves every room.
type Media interface {
	// Join enters room as the whiteboard (Identity, an agent participant)
	// and tells h who's there and what they say until Leave.
	Join(ctx context.Context, room string, h Hearing) (MediaRoom, error)
}

// Hearing is what a room's media tells the whiteboard. Its calls come on
// the media's own goroutines and return at once; none is about an agent
// (the whiteboard itself). All three are set.
type Hearing struct {
	// Arrived is someone in the room: everyone there at the join, then
	// each person who comes.
	Arrived func(identity, name string)
	// Left is someone gone.
	Left func(identity string)
	// Voice hands over a person's voice as it's published: audio only,
	// never a camera or a screen. It's read until it ends.
	Voice func(identity, name string, track VoiceTrack)
}

// MediaRoom is the whiteboard in one room's media.
type MediaRoom interface {
	// Publish sends one 20 ms Opus packet of the whiteboard's voice at RTP
	// timestamp ts.
	Publish(packet []byte, ts uint32) error
	// People is how many people (not agents) are in the room.
	People() int
	Leave()
}

// VoiceTrack is one person's voice as the ears read it (ears.go): RTP
// packets, a read deadline so time going by is noticed when nothing comes
// (a timeout is a net.Error), and the codec. A *webrtc.TrackRemote is one,
// and so is the builtin SFU's *sfu.Voice.
type VoiceTrack interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(time.Time) error
	Codec() webrtc.RTPCodecParameters
}

var _ VoiceTrack = (*webrtc.TrackRemote)(nil)

// join enters the room's media as the whiteboard: it keeps people's names,
// and listens to each voice it's handed.
func (a *Agent) join(ctx context.Context) (MediaRoom, error) {
	if a.cfg.Media == nil {
		return nil, errors.New("agent: no media server")
	}
	return a.cfg.Media.Join(ctx, a.cfg.Room, Hearing{
		Arrived: func(identity, name string) {
			a.setName(identity, name)
			a.log.Info("agent: arrived", "identity", identity)
		},
		Left: func(identity string) { a.log.Info("agent: left", "identity", identity) },
		Voice: func(identity, name string, track VoiceTrack) {
			a.setName(identity, name)
			go a.listen(ctx, track, identity, a.name(identity))
		},
	})
}
