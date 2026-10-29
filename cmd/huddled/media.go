package main

// The media server a meeting's voices and videos go through: the SFU inside
// huddled (internal/server/sfu). serve and agent-check set it up the same
// way, here.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/server/sfu"
)

// media is the media server, set up: what the dev join hands out and /rtc
// serves (api), and how the whiteboard joins a room (agent).
type media struct {
	// nodeIP is the address the SFU offers (HUDDLE_NODE_IP; make server
	// passes LAN_IP), and port its UDP port: the HTTP port's number.
	nodeIP string
	port   int

	tokens rtctoken.Minter // the SFU's tokens (HUDDLE_RTC_SECRET)
	sfu    *sfu.Server

	agent agent.Media
}

// newMedia sets up the SFU for a server listening on addr.
func newMedia(addr string) (*media, error) {
	m := &media{nodeIP: os.Getenv("HUDDLE_NODE_IP")}
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
	if m.sfu, err = sfu.New(sfu.Config{Network: sfu.Network{Port: m.port, IP: m.nodeIP}, Tokens: m.tokens}); err != nil {
		return nil, err
	}
	// The whiteboard joins the SFU in-process.
	m.agent = agent.Builtin{SFU: m.sfu}
	return m, nil
}

// rtcSecret signs the SFU's tokens: HUDDLE_RTC_SECRET, or random for this
// process (a dev server's tokens are minted and checked in one process,
// and last minutes).
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

// api gives the HTTP API the media server's part: the dev join's tokens,
// and the SFU's signalling.
func (m *media) api(s *httpapi.Server) {
	s.Minter, s.RTC = m.tokens, m.sfu
}

// attrs name the media server in the log.
func (m *media) attrs() []any {
	return []any{"nodeIP", m.nodeIP, "mediaPort", m.port, "mediaAddrs", m.sfu.Addrs()}
}

// close tells everyone in the SFU's rooms bye.
func (m *media) close() { m.sfu.Close() }
