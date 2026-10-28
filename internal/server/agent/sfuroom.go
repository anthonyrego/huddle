package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/server/sfu"
)

// Builtin is the whiteboard's Media on huddled's own SFU, which it joins
// in-process (sfu.LocalJoin): no WebSocket, no connections, nothing to
// reconnect. The SFU hands it each person's voice as RTP (an sfu.Voice,
// which the ears read as they read a TrackRemote: voices only, never a
// camera, a screen or another agent), and its voice's packets go straight
// into the room.
type Builtin struct {
	SFU *sfu.Server
}

var _ VoiceTrack = (*sfu.Voice)(nil)

// sfuRoom is the whiteboard in one room of the SFU.
type sfuRoom struct {
	l     *sfu.Local
	voice *sfu.LocalTrack
}

// Join enters the room as the whiteboard, an agent participant, and
// publishes its voice.
func (b Builtin) Join(_ context.Context, room string, h Hearing) (MediaRoom, error) {
	if b.SFU == nil {
		return nil, errors.New("agent: no SFU")
	}
	l, err := b.SFU.LocalJoin(rtctoken.Grant{Room: room, Identity: Identity, Name: "Whiteboard", Agent: true}, sfu.LocalEvents{
		Arrived: h.Arrived,
		Left:    h.Left,
		Voice:   func(identity, name string, v *sfu.Voice) { h.Voice(identity, name, v) },
	})
	if err != nil {
		return nil, fmt.Errorf("agent: join the SFU: %w", err)
	}
	voice, _, err := l.PublishAudio("whiteboard voice")
	if err != nil {
		l.Leave()
		return nil, fmt.Errorf("agent: publish the voice: %w", err)
	}
	return &sfuRoom{l: l, voice: voice}, nil
}

// Publish writes one 20 ms Opus packet of the voice at RTP timestamp ts.
func (r *sfuRoom) Publish(pkt []byte, ts uint32) error { return r.voice.WriteFrame(pkt, ts, nil) }

// People counts the people (not agents) in the room.
func (r *sfuRoom) People() int { return r.l.People() }

func (r *sfuRoom) Leave() { r.l.Leave() }
