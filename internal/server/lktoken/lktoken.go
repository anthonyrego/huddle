// Package lktoken mints LiveKit access tokens. The API secret stays on the
// server; apps get a token for one room and one identity.
package lktoken

import (
	"errors"
	"time"

	"github.com/livekit/protocol/auth"
)

// Minter signs tokens with one API key and secret.
type Minter struct {
	Key, Secret string
	// TTL is how long a token can be used to join. LiveKit tokens can't be
	// revoked, so it stays short; a joined participant isn't cut off when
	// it runs out.
	TTL time.Duration
}

// Grant is who may join which room, and what they may do there.
type Grant struct {
	Room, Identity, Name string
	// Agent marks the whiteboard: it may publish its voice and data, and
	// shows as an agent to others.
	Agent bool
}

// Mint returns a signed token.
func (m Minter) Mint(g Grant) (string, error) {
	if m.Key == "" || m.Secret == "" {
		return "", errors.New("lktoken: no LiveKit API key and secret")
	}
	if g.Room == "" || g.Identity == "" {
		return "", errors.New("lktoken: a token needs a room and an identity")
	}
	ttl := m.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	yes := true
	grant := &auth.VideoGrant{
		RoomJoin:       true,
		Room:           g.Room,
		CanPublish:     &yes,
		CanSubscribe:   &yes,
		CanPublishData: &yes,
		Agent:          g.Agent,
	}
	return auth.NewAccessToken(m.Key, m.Secret).
		SetIdentity(g.Identity).
		SetName(g.Name).
		SetVideoGrant(grant).
		SetValidFor(ttl).
		ToJWT()
}
