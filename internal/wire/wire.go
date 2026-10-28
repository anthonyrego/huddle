// Package wire is what the app and the server say to each other over the
// meeting's WebSocket, and the dev join over HTTP. Both sides import it, so
// a message has one definition.
//
// Every frame is a JSON Envelope. The server is the only one that changes
// the board: the app sends commands and receives versions.
package wire

import (
	"encoding/json"
	"fmt"
)

// Protocol is the version both sides speak. The server refuses a hello
// with another one, so an old app says "please update" instead of
// misreading messages.
const Protocol = 1

// Kinds of message.
const (
	KindHello        = "hello"              // app → server, first frame
	KindWelcome      = "welcome"            // server → app, answers hello
	KindBoardVersion = "board.version"      // server → app: the whole board at a version
	KindBoardOps     = "board.ops"          // server → app: the ops that made a version, to reveal
	KindAgentStatus  = "agent.status"       // server → app: what the whiteboard is doing
	KindCommand      = "command.typed"      // app → server: a typed command for the whiteboard
	KindAddress      = "command.address"    // app → server: push-to-address held or released
	KindTranscript   = "transcript.segment" // server → app: a line of the meeting's transcript
	KindError        = "error"              // either way: something was refused
)

// Envelope is one WebSocket frame: a kind and its body.
type Envelope struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Encode wraps a body in an envelope.
func Encode(kind string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("wire: encode %s: %w", kind, err)
	}
	return json.Marshal(Envelope{Kind: kind, Body: b})
}

// Decode reads an envelope's body into v.
func (e Envelope) Decode(v any) error {
	if err := json.Unmarshal(e.Body, v); err != nil {
		return fmt.Errorf("wire: decode %s: %w", e.Kind, err)
	}
	return nil
}

// Hello opens a meeting connection.
type Hello struct {
	Protocol int    `json:"protocol"`
	Room     string `json:"room"`
	Identity string `json:"identity"` // the participant identity the LiveKit token names
}

// Welcome answers a hello with where the meeting stands.
type Welcome struct {
	Protocol int    `json:"protocol"`
	Room     string `json:"room"`
	Head     int64  `json:"head"` // the board's newest version; a BoardVersion follows when > 0
}

// BoardVersion is the board as of one version, with the layout the server
// computed, so every app (and a replay) draws the same picture.
type BoardVersion struct {
	Room      string          `json:"room"`
	Seq       int64           `json:"seq"`
	Parent    int64           `json:"parent"`
	Requester string          `json:"requester,omitempty"` // who asked for it
	Command   string          `json:"command,omitempty"`   // the command id it answered
	TMs       int64           `json:"t_ms"`                // ms since the meeting started
	Doc       json.RawMessage `json:"doc"`                 // an internal/board Snapshot: the ops that rebuild it
	Layout    json.RawMessage `json:"layout,omitempty"`    // positions and sizes, from the server
}

// BoardOps are the ops that turned version Seq-1 into Seq, in order, for
// the reveal animation.
type BoardOps struct {
	Room string            `json:"room"`
	Seq  int64             `json:"seq"`
	Ops  []json.RawMessage `json:"ops"`
}

// AgentState is what the whiteboard is doing, for the status line.
type AgentState string

const (
	AgentIdle      AgentState = "idle"
	AgentListening AgentState = "listening" // the gate is open: someone is addressing it
	AgentWorking   AgentState = "working"   // a command is with the model
	AgentSpeaking  AgentState = "speaking"
	AgentAuto      AgentState = "auto" // diagramming as the meeting goes
	AgentOffline   AgentState = "offline"
)

// AgentStatus is the whiteboard's state, with the queue of commands
// waiting behind the one in flight.
type AgentStatus struct {
	Room   string     `json:"room"`
	State  AgentState `json:"state"`
	Detail string     `json:"detail,omitempty"` // e.g. "for Sam: draw the flow"
	Queued int        `json:"queued,omitempty"`
}

// Command is a typed instruction to the whiteboard.
type Command struct {
	Room string `json:"room"`
	Text string `json:"text"`
}

// Address is the push-to-address key: while held, what the person says is
// for the whiteboard.
type Address struct {
	Room string `json:"room"`
	Held bool   `json:"held"`
}

// Segment is one line of the transcript.
type Segment struct {
	Room     string `json:"room"`
	Speaker  string `json:"speaker"` // the participant's display name, or "Whiteboard"
	Identity string `json:"identity"`
	StartMs  int64  `json:"start_ms"` // ms since the meeting started
	EndMs    int64  `json:"end_ms"`
	Text     string `json:"text"`
	Final    bool   `json:"final"` // false while it's still being said
}

// Error is a refusal.
type Error struct {
	Message string `json:"message"`
}

// DevJoin asks a dev server (HUDDLE_DEV=1) to let a name into a room. Stage
// 3 replaces it with signing in.
type DevJoin struct {
	Room string `json:"room"`
	Name string `json:"name"`
}

// Joined answers a join with what the app needs to connect: a LiveKit
// token for the media and the URL of the meeting's WebSocket.
type Joined struct {
	Room       string `json:"room"`
	Identity   string `json:"identity"`
	LiveKitURL string `json:"livekit_url"`
	Token      string `json:"token"`
	WSURL      string `json:"ws_url"`
}
