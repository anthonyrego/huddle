// Package rtctoken signs and checks the tokens a client joins huddle's SFU
// with: one room, one identity, short-lived. A token is its claims in JSON
// and an HMAC-SHA256 of them, each base64url, joined by a dot; the standard
// library only. The dev join mints them (stage 3's sign-in will too). Pure.
package rtctoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Minter signs and checks tokens with one secret.
type Minter struct {
	Secret []byte
	// TTL is how long a token can be used to join; a joined session isn't
	// cut off when it runs out. Zero is 10 minutes.
	TTL time.Duration
}

// Grant is who a token lets in, and where.
type Grant struct {
	Room, Identity, Name string
	// Agent marks the whiteboard; it shows as an agent to others.
	Agent bool
}

type claims struct {
	Room     string `json:"room"`
	Identity string `json:"id"`
	Name     string `json:"name,omitempty"`
	Agent    bool   `json:"agent,omitempty"`
	Expires  int64  `json:"exp"` // Unix seconds
}

var (
	ErrNoSecret = errors.New("rtctoken: no secret")
	ErrBad      = errors.New("rtctoken: not a valid token")
	ErrExpired  = errors.New("rtctoken: expired")
)

var b64 = base64.RawURLEncoding

// Mint signs a token for g, good until TTL from now.
func (m Minter) Mint(g Grant) (string, error) {
	if len(m.Secret) == 0 {
		return "", ErrNoSecret
	}
	if g.Room == "" || g.Identity == "" {
		return "", errors.New("rtctoken: a token needs a room and an identity")
	}
	ttl := m.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	body, err := json.Marshal(claims{g.Room, g.Identity, g.Name, g.Agent, time.Now().Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	p := b64.EncodeToString(body)
	return p + "." + b64.EncodeToString(m.sign(p)), nil
}

// Verify checks a token's signature and that it hasn't expired at now, and
// says what it grants.
func (m Minter) Verify(token string, now time.Time) (Grant, error) {
	if len(m.Secret) == 0 {
		return Grant{}, ErrNoSecret
	}
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Grant{}, ErrBad
	}
	got, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(got, m.sign(p)) {
		return Grant{}, ErrBad
	}
	body, err := b64.DecodeString(p)
	if err != nil {
		return Grant{}, ErrBad
	}
	var c claims
	if err := json.Unmarshal(body, &c); err != nil || c.Room == "" || c.Identity == "" {
		return Grant{}, ErrBad
	}
	if now.Unix() >= c.Expires {
		return Grant{}, ErrExpired
	}
	return Grant{Room: c.Room, Identity: c.Identity, Name: c.Name, Agent: c.Agent}, nil
}

func (m Minter) sign(payload string) []byte {
	h := hmac.New(sha256.New, m.Secret)
	h.Write([]byte(payload))
	return h.Sum(nil)
}
