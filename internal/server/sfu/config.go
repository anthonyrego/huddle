// Package sfu is huddle's media server, inside huddled serve: people publish
// their voice, camera and screen to it, and it forwards each to everyone
// else, choosing a simulcast layer for each viewer. It replaces LiveKit's
// server, adapted from it where noted (Apache 2.0; see NOTICE). Pure.
package sfu

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Network is where the SFU's media goes: one UDP port for every
// connection, on this machine's addresses.
type Network struct {
	// Port is the UDP port (0: a free one, as tests want).
	Port int
	// IP, when set, is the one address candidates are offered on: this
	// Mac's LAN address (make server passes LAN_IP), or 127.0.0.1 for
	// everything on one Mac. Empty: every address but those of virtual
	// interfaces (VPN tunnels, AirDrop, bridges).
	IP string
	// External, when set, is offered in place of the address: what a NAT
	// in front of the server forwards the port from.
	External string
	// Loopback offers 127.0.0.1 too (tests: everything on one machine).
	Loopback bool
}

// Connection settings both ends use (livekit/server-sdk-go v2.19.0
// transport.go's values).
const (
	dtlsRetransmission = 100 * time.Millisecond
	iceDisconnected    = 10 * time.Second
	iceFailed          = 5 * time.Second
	iceKeepalive       = 2 * time.Second
)

// virtual names the interfaces media isn't offered on: VPN tunnels,
// AirDrop and its low-latency link, bridges, and the like.
var virtual = []string{"utun", "awdl", "llw", "bridge", "anpi", "gif", "stf", "vmenet", "docker", "veth"}

func keepInterface(name string) bool {
	for _, p := range virtual {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// network is the SFU's one UDP socket per address, shared by every
// connection.
type network struct {
	cfg Network
	mux *ice.MultiUDPMuxDefault
}

func listen(cfg Network) (*network, error) {
	if ip := net.ParseIP(cfg.IP); ip != nil && ip.IsLoopback() {
		cfg.Loopback = true
	}
	keepIP := func(ip net.IP) bool {
		if ip.IsLoopback() {
			return cfg.Loopback
		}
		return cfg.IP == "" || ip.Equal(net.ParseIP(cfg.IP))
	}
	opts := []ice.UDPMuxFromPortOption{
		ice.UDPMuxFromPortWithInterfaceFilter(keepInterface),
		ice.UDPMuxFromPortWithIPFilter(keepIP),
		ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4),
	}
	if cfg.Loopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback())
	}
	mux, err := ice.NewMultiUDPMuxFromPort(cfg.Port, opts...)
	if err != nil {
		return nil, fmt.Errorf("media port %d: %w", cfg.Port, err)
	}
	return &network{cfg: cfg, mux: mux}, nil
}

// addrs are the addresses media arrives on.
func (n *network) addrs() []net.Addr { return n.mux.GetListenAddresses() }

func (n *network) close() error { return n.mux.Close() }

// settings are the SettingEngine every SFU connection is made with.
func (n *network) settings() (webrtc.SettingEngine, error) {
	var s webrtc.SettingEngine
	s.SetICEUDPMux(n.mux)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	s.SetIncludeLoopbackCandidate(n.cfg.Loopback)
	s.SetInterfaceFilter(keepInterface)
	if n.cfg.External != "" {
		err := s.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{n.cfg.External},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		if err != nil {
			return s, fmt.Errorf("advertise %s: %w", n.cfg.External, err)
		}
	}
	s.SetSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80)
	s.SetDTLSRetransmissionInterval(dtlsRetransmission)
	s.SetICETimeouts(iceDisconnected, iceFailed, iceKeepalive)
	return s, nil
}
