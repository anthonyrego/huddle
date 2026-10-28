package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/server/lktoken"
)

// lkRoom is the agent's place in the room's LiveKit session: it hears
// every person's audio track and publishes its own voice.
type lkRoom struct {
	room  *lksdk.Room
	track *lksdk.LocalTrack

	mu     sync.Mutex
	people map[string]bool // identities of the people (not agents) in the room
}

// join enters the room as the whiteboard, an agent participant.
func (a *Agent) join(ctx context.Context) (*lkRoom, error) {
	m := a.cfg.Minter
	m.TTL = 12 * time.Hour // it may need to rejoin long after it started
	tok, err := m.Mint(lktoken.Grant{Room: a.cfg.Room, Identity: Identity, Name: "Whiteboard", Agent: true})
	if err != nil {
		return nil, err
	}
	r := &lkRoom{people: map[string]bool{}}
	cb := lksdk.NewRoomCallback()
	cb.OnParticipantConnected = func(rp *lksdk.RemoteParticipant) {
		if rp.Kind() != lksdk.ParticipantAgent {
			r.set(rp.Identity(), true)
			a.setName(rp.Identity(), rp.Name())
		}
	}
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) { r.set(rp.Identity(), false) }
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio || rp.Kind() == lksdk.ParticipantAgent {
			return
		}
		a.setName(rp.Identity(), rp.Name())
		r.set(rp.Identity(), true)
		go a.listen(ctx, track, rp.Identity(), a.name(rp.Identity()))
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
				a.log.Warn("agent: subscribe", "identity", rp.Identity(), "err", err)
			}
		}()
	}
	room, err := lksdk.ConnectToRoomWithToken(a.cfg.LiveKitURL, tok, cb, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return nil, fmt.Errorf("agent: join LiveKit: %w", err)
	}
	r.room = room
	for _, rp := range room.GetRemoteParticipants() {
		if rp.Kind() != lksdk.ParticipantAgent {
			r.set(rp.Identity(), true)
			a.setName(rp.Identity(), rp.Name())
		}
	}
	r.track, err = lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err != nil {
		room.Disconnect()
		return nil, err
	}
	if _, err := room.LocalParticipant.PublishTrack(r.track, &lksdk.TrackPublicationOptions{Name: "whiteboard voice"}); err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("agent: publish the voice: %w", err)
	}
	return r, nil
}

func (r *lkRoom) set(identity string, in bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if in {
		r.people[identity] = true
	} else {
		delete(r.people, identity)
	}
}

// count is how many people are in the room.
func (r *lkRoom) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.people)
}

// publish writes one 20 ms Opus packet to the voice track at RTP
// timestamp ts.
func (r *lkRoom) publish(pkt []byte, ts uint32) error {
	return r.track.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond, PacketTimestamp: ts}, nil)
}

func (r *lkRoom) leave() { r.room.Disconnect() }
