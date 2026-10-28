// Package rtc is the app's side of a meeting's media: joining the LiveKit
// room, sending the microphone and hearing everyone else. Track V
// implements it with LiveKit's Go SDK (pion, no cgo); the app only sees
// this interface, so the board stage and the voice session grow apart.
//
// Audio crosses this boundary as 48 kHz mono float32 PCM in 10 ms frames
// (480 samples). Opus, jitter buffers and mixing live behind it.
package rtc

import (
	"context"
	"time"
)

// SampleRate and FrameSamples fix the PCM that crosses the interface.
const (
	SampleRate   = 48000
	FrameSamples = SampleRate / 100 // 10 ms
)

// Join is what the server's join answer gives the app.
type Join struct {
	URL      string // LiveKit's WebSocket URL
	Token    string // a LiveKit access token for one room and identity
	Identity string
}

// Participant is someone else in the room.
type Participant struct {
	Identity string
	Name     string
	Agent    bool // the whiteboard
}

// Level is how loud a participant is right now, for talking indicators.
type Level struct {
	Identity string
	DBFS     float32
}

// Stats are a room's media numbers for the log and the gates.
type Stats struct {
	// JitterDelay is each remote participant's jitter-buffer target.
	JitterDelay map[string]time.Duration
	// Lost and Concealed count packets missing and frames made up (FEC or
	// PLC) since joining, per remote participant.
	Lost, Concealed map[string]int
}

// Events are what a room tells the app. They're called on the room's own
// goroutines: hand work to the main thread, don't draw from them.
type Events struct {
	Joined       func(self Participant, others []Participant)
	Arrived      func(Participant)
	Left         func(identity string)
	Levels       func([]Level)
	Disconnected func(err error) // the room ended or the connection was lost for good
	// Reconnecting and Reconnected bracket the SDK's own attempt to get a
	// dropped connection back; Disconnected follows if it can't.
	Reconnecting, Reconnected func()
}

// Mic is where the room takes the local voice from: 10 ms frames, already
// echo-cancelled. Read blocks until a frame is ready or ctx ends.
type Mic interface {
	Read(ctx context.Context, frame []float32) error
}

// Speaker is where the room puts everyone else's voice, mixed: Write
// queues one 10 ms frame and blocks while the device has enough queued.
type Speaker interface {
	Write(ctx context.Context, frame []float32) error
}

// Room is one joined meeting.
type Room interface {
	// PublishMic starts sending the voice read from mic; Mute stops it
	// without leaving.
	PublishMic(mic Mic) error
	Mute(muted bool)
	// Play starts mixing everyone else into spk.
	Play(spk Speaker) error
	Participants() []Participant
	Stats() Stats
	// Leave hangs up. It is safe to call more than once.
	Leave()
}

// Connector joins rooms. Track V provides the LiveKit one.
type Connector interface {
	Connect(ctx context.Context, j Join, ev Events) (Room, error)
}
