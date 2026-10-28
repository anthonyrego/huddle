package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/server/lktoken"
)

// LiveKit is the whiteboard's Media on a LiveKit server: it joins each room
// with a token it mints with the server's API key.
type LiveKit struct {
	Minter lktoken.Minter
	URL    string // LiveKit's URL as huddled reaches it (ws://host:7880)
}

// lkRoom is the whiteboard in one LiveKit room: it hears every person's
// audio track and publishes its own voice.
type lkRoom struct {
	room  *lksdk.Room
	track *lksdk.LocalTrack
}

// Join enters the room as the whiteboard, an agent participant.
func (l LiveKit) Join(ctx context.Context, room string, h Hearing) (MediaRoom, error) {
	m := l.Minter
	m.TTL = 12 * time.Hour // it may need to rejoin long after it started
	tok, err := m.Mint(lktoken.Grant{Room: room, Identity: Identity, Name: "Whiteboard", Agent: true})
	if err != nil {
		return nil, err
	}
	r := &lkRoom{}
	cb := lksdk.NewRoomCallback()
	cb.OnParticipantConnected = func(rp *lksdk.RemoteParticipant) {
		if rp.Kind() != lksdk.ParticipantAgent {
			h.Arrived(rp.Identity(), rp.Name())
		}
	}
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) {
		if rp.Kind() != lksdk.ParticipantAgent {
			h.Left(rp.Identity())
		}
	}
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio || rp.Kind() == lksdk.ParticipantAgent {
			return
		}
		h.Voice(rp.Identity(), rp.Name(), track)
	}
	// Audio only: the whiteboard never shows video, so it subscribes to
	// each audio track as it's published (the ones there at join come the
	// same way) and the SFU never sends it a camera or a screen.
	cb.OnTrackPublished = func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if pub.Kind() != lksdk.TrackKindAudio || rp.Kind() == lksdk.ParticipantAgent {
			return
		}
		go func() {
			if err := pub.SetSubscribed(true); err != nil {
				slog.Warn("agent: subscribe", "room", room, "identity", rp.Identity(), "err", err)
			}
		}()
	}
	r.room, err = lksdk.ConnectToRoomWithToken(l.URL, tok, cb, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return nil, fmt.Errorf("agent: join LiveKit: %w", err)
	}
	for _, rp := range r.room.GetRemoteParticipants() {
		if rp.Kind() != lksdk.ParticipantAgent {
			h.Arrived(rp.Identity(), rp.Name())
		}
	}
	r.track, err = lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err != nil {
		r.room.Disconnect()
		return nil, err
	}
	if _, err := r.room.LocalParticipant.PublishTrack(r.track, &lksdk.TrackPublicationOptions{Name: "whiteboard voice"}); err != nil {
		r.room.Disconnect()
		return nil, fmt.Errorf("agent: publish the voice: %w", err)
	}
	return r, nil
}

// People counts the remote participants who aren't agents.
func (r *lkRoom) People() int {
	n := 0
	for _, rp := range r.room.GetRemoteParticipants() {
		if rp.Kind() != lksdk.ParticipantAgent {
			n++
		}
	}
	return n
}

// Publish writes one 20 ms Opus packet to the voice track at RTP timestamp
// ts.
func (r *lkRoom) Publish(pkt []byte, ts uint32) error {
	return r.track.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond, PacketTimestamp: ts}, nil)
}

func (r *lkRoom) Leave() { r.room.Disconnect() }
