package main

// The media server a meeting's voices and videos go through (HUDDLE_MEDIA):
// livekit, a LiveKit server of its own (the default until huddled's own SFU
// matches its numbers), or builtin, the SFU inside huddled. serve and
// agent-check set it up the same way, here.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/lktoken"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/server/sfu"
	"github.com/anthonyrego/huddle/internal/wire"
)

// media is the media server, set up: what the dev join hands out and /rtc
// serves (api), and how the whiteboard joins a room (agent).
type media struct {
	kind string // wire.MediaLiveKit or wire.MediaBuiltin
	// nodeIP is the address the builtin SFU offers (HUDDLE_NODE_IP; make
	// server passes LAN_IP), and port its UDP port: the HTTP port's number.
	nodeIP string
	port   int

	lkURL  string          // LiveKit's URL as apps reach it (LIVEKIT_URL)
	lk     lktoken.Minter  // LiveKit's tokens (LIVEKIT_API_KEY and _SECRET)
	tokens rtctoken.Minter // the builtin SFU's tokens (HUDDLE_RTC_SECRET)
	rtc    http.Handler    // the builtin SFU's signalling, at /rtc
	sfu    *sfu.Server     // the builtin SFU

	agent agent.Media
	close func()
}

// newMedia sets up the media server HUDDLE_MEDIA names, for a server
// listening on addr.
func newMedia(addr string) (*media, error) {
	m := &media{kind: env("HUDDLE_MEDIA", wire.MediaLiveKit), nodeIP: os.Getenv("HUDDLE_NODE_IP"), close: func() {}}
	switch m.kind {
	case wire.MediaLiveKit:
		m.lkURL = os.Getenv("LIVEKIT_URL")
		m.lk = lktoken.Minter{Key: os.Getenv("LIVEKIT_API_KEY"), Secret: os.Getenv("LIVEKIT_API_SECRET"), TTL: 10 * time.Minute}
		if m.lkURL == "" || m.lk.Key == "" || m.lk.Secret == "" {
			return nil, errors.New("HUDDLE_MEDIA=livekit needs LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET (make server sets them)")
		}
		m.agent = agent.LiveKit{Minter: m.lk, URL: m.lkURL}
		return m, nil
	case wire.MediaBuiltin:
		if m.nodeIP != "" && net.ParseIP(m.nodeIP) == nil {
			return nil, fmt.Errorf("HUDDLE_NODE_IP is %q, not an IP address", m.nodeIP)
		}
		_, port, err := net.SplitHostPort(addr)
		if err == nil {
			m.port, err = strconv.Atoi(port)
		}
		if err != nil {
			return nil, fmt.Errorf("the media port follows the listen address's, and %q has none: %w", addr, err)
		}
		secret, err := rtcSecret()
		if err != nil {
			return nil, err
		}
		m.tokens = rtctoken.Minter{Secret: secret, TTL: 10 * time.Minute}
		srv, err := sfu.New(sfu.Config{Network: sfu.Network{Port: m.port, IP: m.nodeIP}, Tokens: m.tokens})
		if err != nil {
			return nil, err
		}
		m.rtc, m.sfu = srv, srv
		m.close = func() { srv.Close() }
		// The whiteboard joins huddled's SFU in-process.
		m.agent = agent.Builtin{SFU: srv}
		return m, nil
	default:
		return nil, fmt.Errorf("HUDDLE_MEDIA is %q: livekit or builtin", m.kind)
	}
}

// rtcSecret signs the builtin SFU's tokens: HUDDLE_RTC_SECRET, or random
// for this process (a dev server's tokens are minted and checked in one
// process, and last minutes).
func rtcSecret() ([]byte, error) {
	if s := os.Getenv("HUDDLE_RTC_SECRET"); s != "" {
		if len(s) < 32 {
			return nil, errors.New("HUDDLE_RTC_SECRET is shorter than 32 characters")
		}
		return []byte(s), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// api gives the HTTP API the media server's part: what the dev join hands
// out, and the SFU's signalling.
func (m *media) api(s *httpapi.Server) {
	s.Media, s.Minter, s.LiveKitURL, s.RTCMinter, s.RTC = m.kind, m.lk, m.lkURL, m.tokens, m.rtc
}

// attrs name the media server in the log.
func (m *media) attrs() []any {
	a := []any{"media", m.kind, "nodeIP", m.nodeIP}
	if m.kind == wire.MediaLiveKit {
		return append(a, "livekit", m.lkURL)
	}
	a = append(a, "mediaPort", m.port)
	if m.sfu != nil {
		a = append(a, "mediaAddrs", m.sfu.Addrs())
	}
	return a
}
