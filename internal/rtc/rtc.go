// Package rtc is the app's side of a meeting's media: joining the room on
// the media server, sending the microphone and camera, and hearing and
// seeing everyone else. internal/rtc/room implements it on huddled's own
// SFU (pion, no cgo); the app only sees this interface, so the board stage
// and the call grow apart.
//
// Audio crosses this boundary as 48 kHz mono float32 PCM in 10 ms frames
// (480 samples). Opus, jitter buffers and mixing live behind it.
package rtc

import (
	"context"
	"time"

	"github.com/anthonyrego/huddle/internal/video"
)

// SampleRate and FrameSamples fix the PCM that crosses the interface.
const (
	SampleRate   = 48000
	FrameSamples = SampleRate / 100 // 10 ms
)

// Join is what the server's join answer gives the app.
type Join struct {
	URL      string // the media server's WebSocket URL
	Token    string // its token for one room and identity
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
	// Reconnecting and Reconnected bracket a transport's own attempt to get
	// a dropped connection back; Disconnected follows if it can't. The room
	// on huddled's SFU has no resume, so neither fires and Disconnected
	// comes at once.
	Reconnecting, Reconnected func()
	// VideoAdded and VideoRemoved bring each remote camera and screen as
	// it comes and goes.
	VideoAdded, VideoRemoved func(VideoTrack)
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
	// PublishVideo starts sending a camera or a screen; the returned
	// publication stops it.
	PublishVideo(src VideoSource, kind VideoKind) (VideoPub, error)
	// Leave hangs up. It is safe to call more than once.
	Leave()
}

// Connector joins rooms (internal/rtc/room).
type Connector interface {
	Connect(ctx context.Context, j Join, ev Events) (Room, error)
}

// Video crosses this boundary encoded on the way out and decoded on the
// way in: the platform's encoder and decoder (VideoToolbox on macOS) stay
// on the app's side, so the room's code builds everywhere. Frames are
// video.Frame (NV12 in Go memory); access units are H.264 Annex-B.

// VideoKind is what a video shows.
type VideoKind int

const (
	Camera VideoKind = iota
	Screen
)

func (k VideoKind) String() string {
	if k == Screen {
		return "screen"
	}
	return "camera"
}

// Layer is one simulcast layer a video is sent at.
type Layer struct {
	W, H    int
	Bitrate int // bits a second at full rate
}

// VideoSource is a local video, encoded at one or more layers (largest
// first). The room publishes the layers and tells the source what the
// network asks of it.
type VideoSource interface {
	Layers() []Layer
	// Start begins delivering each layer's access units to write, from
	// the source's own goroutines, until Stop.
	Start(write func(layer int, au []byte, pts time.Duration)) error
	// KeyFrame asks a layer for a keyframe (a receiver's PLI or FIR).
	KeyFrame(layer int)
	// SetBitrates gives each layer the bits a second it may use now, from
	// congestion control; 0 pauses a layer.
	SetBitrates(bps []int)
	Stop()
}

// VideoPub is a published video.
type VideoPub interface {
	Stop()
}

// VideoDecoder turns one remote track's access units into frames. It
// reports false when a unit made no picture (parameter sets alone).
type VideoDecoder interface {
	Decode(au []byte, f *video.Frame) (bool, error)
	Close()
}

// VideoTrack is a remote camera or screen, decoded.
type VideoTrack interface {
	Identity() string // whose it is
	Kind() VideoKind
	// Take calls fn with the newest frame, under the track's lock, if it's
	// newer than after (0 for any); it returns that frame's seq.
	Take(after uint64, fn func(video.Frame)) (uint64, bool)
	// SetSize says how big the track is shown, in pixels, so the SFU can
	// send the layer that fits (0, 0: not shown).
	SetSize(w, h int)
}
