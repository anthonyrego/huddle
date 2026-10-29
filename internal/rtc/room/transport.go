package room

// The transport is the media server under a room: LiveKit through its Go
// SDK (lk.go), or huddle's own SFU through sfuclient (sfu.go). It carries everything the room says to the server or hears
// from it: who's there, the tracks it's sent and what it asks of them, what
// it publishes, and the congestion control on the connection it publishes
// on. Opus, jitter buffers, the mix, the allocator, the assembler, decoding
// and lip sync are the room's, whichever transport it's on.

import (
	"context"
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/wire"
)

// transport is one joined session with a media server.
type transport interface {
	// self is who we joined as, roomName the room's name.
	self() rtc.Participant
	roomName() string
	// participants is everyone else there now.
	participants() []rtc.Participant
	// publishAudio publishes the mic.
	publishAudio() (audioOut, error)
	// publishVideo publishes a camera or a screen as one simulcast track, a
	// layer per entry of layers (largest first; they share one RTP
	// timestamp base). onRTCP hears what receivers say about each layer
	// (a PLI or FIR asks that layer's encoder for a keyframe).
	publishVideo(kind rtc.VideoKind, layers []rtc.Layer, onRTCP func(layer int, p rtcp.Packet)) (videoOut, error)
	// estimator is the congestion control on the connection we publish
	// on; nil until that connection exists.
	estimator() *icept.Estimator
	// leave hangs up.
	leave()
}

// dialFunc joins the room j is for. ev hears what happens from the moment
// it's called, on the transport's goroutines, until leave.
type dialFunc func(ctx context.Context, j rtc.Join, cfg dialConfig, ev transportEvents) (transport, error)

// dialConfig is what the room asks of a transport's connections.
type dialConfig struct {
	// send and recv are the room's interceptors for the connection we send
	// on and the one we receive on, nearest the network first. After send,
	// the transport puts congestion control (icept.NewCC, and whatever its
	// feedback needs) on the connection it publishes on. A transport that
	// can't keep the two apart puts both lists on every connection, recv's
	// first.
	send, recv []interceptor.Factory
	// subscribe is whether to be sent everyone else's tracks (false: none;
	// a bot that only publishes).
	subscribe bool
}

// transportEvents are what a transport tells the room. Every field is set.
type transportEvents struct {
	arrived func(rtc.Participant)
	left    func(identity string)
	// subscribed brings a remote track as we're sent it. It returns what
	// hears the track's RTCP (its sender reports, for lip sync), or nil.
	subscribed   func(remoteTrack) func(rtcp.Packet)
	unsubscribed func(sid string)
	// speakers is who the server hears talking, by the levels their audio
	// carries: by identity, from 0 (quiet) to 1 (loud).
	speakers func(map[string]float32)
	// reconnecting and reconnected bracket the transport's own attempt to
	// get a dropped connection back; disconnected follows if it can't, or
	// the server ends the session (and after leave).
	reconnecting, reconnected func()
	disconnected              func(reason string)
}

// remoteTrack is a track we're sent.
type remoteTrack struct {
	sid  string // the server's name for it
	who  string // its publisher's identity
	name string // its publisher's name for it
	kind webrtc.RTPCodecType
	// mime is its codec's MIME type, as negotiated (a TrackRemote's own
	// Codec is empty until its first packet is read).
	mime   string
	source source
	track  *webrtc.TrackRemote
	// levelExt is the RFC 6464 audio level's header extension ID on its
	// packets (0: none).
	levelExt int
	// video is what we ask of the server about a video track (nil for
	// audio).
	video remoteVideo
}

// source is what a track carries.
type source int

const (
	sourceOther source = iota
	sourceMic
	sourceCamera
	sourceScreen
)

// remoteVideo is what a receiver asks of the server about a video track.
type remoteVideo interface {
	// setSize asks for the layer that fits w×h pixels.
	setSize(w, h int)
	// setEnabled turns the track on or off (off: nothing is sent).
	setEnabled(on bool)
	// pli asks the publisher for a keyframe.
	pli()
}

// audioOut is the published mic.
type audioOut interface {
	sid() string
	// write sends one 20 ms Opus packet at RTP timestamp ts, with its RFC
	// 6464 level (-dBov: 0 loud, 127 silence).
	write(packet []byte, ts uint32, level uint8) error
	// setMuted tells the room the mic is muted (nothing is sent) or not.
	setMuted(muted bool)
}

// videoOut is a published camera or screen.
type videoOut interface {
	sid() string
	// write sends one access unit (Annex-B) of a layer at RTP timestamp ts.
	write(layer int, au []byte, ts uint32) error
	unpublish() error
}

// wantedOut is a videoOut whose server says which of its layers anyone is
// sent (Dynacast: huddle's SFU; lksdk doesn't pass LiveKit's on, so there
// every layer stays wanted). f hears each change, a bool per layer, largest
// first, and the last one heard before it was set.
type wantedOut interface {
	onWanted(f func(wanted []bool))
}

// dialer is the transport for a join's media server.
func dialer(kind string) (dialFunc, error) {
	switch kind {
	case "", wire.MediaLiveKit:
		return dialLK, nil
	case wire.MediaBuiltin:
		return dialSFU, nil
	}
	return nil, fmt.Errorf("unknown media server %q", kind)
}
