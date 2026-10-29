// Package signal is what a client and huddle's SFU say on the server's /rtc
// WebSocket (internal/server/sfu, internal/rtc/sfuclient): JSON envelopes,
// as on the meeting WebSocket (internal/wire), with no pion in them.
//
// A session: the client sends join (with the token the dev join gave it);
// the server answers joined (who's there and what they publish), then tells
// of changes (participant, left, track, untrack). Each person has two
// connections. On Pub the client publishes: it sends publish for a track,
// the server answers published, and the client offers (the server answers).
// On Sub the server sends everyone else's tracks: it offers, the client
// answers. The server always sends a track message before the offer that
// carries the track, so a client knows every track it's offered. ICE
// candidates trickle both ways, named by connection. A viewer's settings
// say the size it shows a video at; the video's publisher hears wanted,
// the layers anyone is sent, and pauses the rest (Dynacast).
//
// What arrives on Sub is named so the client can match it: a down track's
// ID is the track's SID and its stream ID is the publisher's identity. What
// the client publishes is matched by its track ID, the publish message's
// CID. A video track's layers are simulcast RIDs, largest first: f, h, q
// (two layers: f and q).
//
// Pure.
package signal

import (
	"encoding/json"
	"fmt"
)

// Protocol is checked on join; a client of another version is turned away
// with an error.
const Protocol = 1

// Envelope is one message: Kind names the type of Body.
type Envelope struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Kinds of message. Offer, Answer and ICE go both ways; the rest go one way,
// as marked.
const (
	KindJoin      = "join"      // client: Join
	KindOffer     = "offer"     // SDP: the client's for Pub, the server's for Sub
	KindAnswer    = "answer"    // SDP
	KindICE       = "ice"       // ICE
	KindPublish   = "publish"   // client: Publish
	KindUnpublish = "unpublish" // client: Unpublish
	KindMute      = "mute"      // client: Mute
	KindSettings  = "settings"  // client: Settings
	KindLeave     = "leave"     // client: no body

	KindJoined      = "joined"      // server: Joined
	KindParticipant = "participant" // server: Participant (arrived, or changed)
	KindLeft        = "left"        // server: Left
	KindTrack       = "track"       // server: TrackInfo (published, or changed)
	KindUntrack     = "untrack"     // server: Untrack
	KindPublished   = "published"   // server: Published
	KindWanted      = "wanted"      // server: Wanted
	KindSpeakers    = "speakers"    // server: Speakers
	KindError       = "error"       // server: Error
	KindBye         = "bye"         // server: Bye, then it closes
)

// PC names one of a person's two connections.
type PC string

const (
	// Pub: the client publishes on it, and offers.
	Pub PC = "pub"
	// Sub: the client receives on it; the server offers.
	Sub PC = "sub"
)

// Subscriptions a client asks for on join.
const (
	SubscribeAll  = "all"  // everyone else's tracks (the default)
	SubscribeNone = "none" // nothing: a bot that only publishes
)

// Join opens a session. Token is from the server's dev join (an rtctoken).
type Join struct {
	Protocol  int    `json:"protocol"`
	Token     string `json:"token"`
	Subscribe string `json:"subscribe,omitempty"`
}

// SDP is an offer or an answer for one connection.
type SDP struct {
	PC  PC     `json:"pc"`
	SDP string `json:"sdp"`
}

// ICE is a trickled candidate for one connection. The fields and their JSON
// names are webrtc.ICECandidateInit's.
type ICE struct {
	PC               PC      `json:"pc"`
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// TrackKind is audio or video.
type TrackKind string

const (
	Audio TrackKind = "audio"
	Video TrackKind = "video"
)

// Source is what a track carries.
type Source string

const (
	Microphone Source = "microphone"
	Camera     Source = "camera"
	Screen     Source = "screen"
)

// Layer is one simulcast layer of a video track.
type Layer struct {
	RID     string `json:"rid"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Bitrate int    `json:"bitrate"` // bits a second at most
}

// Publish asks to publish a track; once published, the client offers Pub
// with it. CID is the client's name for it: its TrackLocal's ID, which the
// server matches to what arrives. A video track's layers are largest first,
// and share one RTP timestamp base (the SFU switches viewers between them
// without rewriting timestamps).
type Publish struct {
	CID    string    `json:"cid"`
	Kind   TrackKind `json:"kind"`
	Source Source    `json:"source"`
	Name   string    `json:"name,omitempty"`
	Layers []Layer   `json:"layers,omitempty"`
	Muted  bool      `json:"muted,omitempty"`
}

// Published answers a publish: the server's SID for the track.
type Published struct {
	CID string `json:"cid"`
	SID string `json:"sid"`
}

// Unpublish stops a track the client published.
type Unpublish struct {
	SID string `json:"sid"`
}

// Mute says a published track is muted (nothing sent) or not.
type Mute struct {
	SID   string `json:"sid"`
	Muted bool   `json:"muted"`
}

// Settings is how a viewer wants one video track: the size it's shown at,
// in pixels (the server sends the smallest layer at least that size, or the
// largest; a layer its publisher has paused is stood in for by the nearest
// that runs), or nothing (Enabled false, or no size). A video track sends
// nothing until its first settings, or for a quarter of a second after
// it's on the receive connection: a viewer that says nothing by then is
// sent the largest layer.
type Settings struct {
	SID     string `json:"sid"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Enabled bool   `json:"enabled"`
}

// Participant is someone in the room, with what they publish.
type Participant struct {
	Identity string      `json:"identity"`
	Name     string      `json:"name"`
	Agent    bool        `json:"agent,omitempty"` // the whiteboard
	Tracks   []TrackInfo `json:"tracks,omitempty"`
}

// TrackInfo is a published track.
type TrackInfo struct {
	SID         string    `json:"sid"`
	Participant string    `json:"participant"` // the publisher's identity
	Kind        TrackKind `json:"kind"`
	Source      Source    `json:"source"`
	Name        string    `json:"name,omitempty"`
	Layers      []Layer   `json:"layers,omitempty"`
	Muted       bool      `json:"muted,omitempty"`
}

// Joined answers a join: who the client is, and everyone else.
type Joined struct {
	Room         string        `json:"room"`
	Self         Participant   `json:"self"`
	Participants []Participant `json:"participants"`
}

// Left says someone left.
type Left struct {
	Identity string `json:"identity"`
}

// Untrack says a track is gone.
type Untrack struct {
	SID string `json:"sid"`
}

// Wanted tells a publisher which layers of one of its video tracks anyone
// is sent (Dynacast): the rest can pause. A publisher sends every layer
// until it hears otherwise. The server sends it at once when the set
// grows, and a few seconds after a layer leaves it. RIDs may be empty:
// nobody is sent the track.
type Wanted struct {
	SID  string   `json:"sid"`
	RIDs []string `json:"rids"`
}

// Speaker is someone talking, by the levels their audio carries (RFC 6464):
// Level from 0 (quiet) to 1 (loud).
type Speaker struct {
	Identity string  `json:"identity"`
	Level    float32 `json:"level"`
}

// Speakers is who's talking now, loudest first; empty when no one is.
type Speakers struct {
	Speakers []Speaker `json:"speakers"`
}

// Error is something the server couldn't do.
type Error struct {
	Message string `json:"message"`
}

// Bye says the server is ending the session (another session took the
// identity; the room closed).
type Bye struct {
	Reason string `json:"reason"`
}

// Marshal makes one message.
func Marshal(kind string, body any) ([]byte, error) {
	e := Envelope{Kind: kind}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("signal: %s: %w", kind, err)
		}
		e.Body = b
	}
	return json.Marshal(e)
}

// Decode reads the envelope's body into v.
func (e Envelope) Decode(v any) error {
	if err := json.Unmarshal(e.Body, v); err != nil {
		return fmt.Errorf("signal: %s: %w", e.Kind, err)
	}
	return nil
}
