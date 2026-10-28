package fixturemeet

// LiveKit's half of the bots' media (JoinMic on HUDDLE_MEDIA=livekit),
// until the media server's step 5 deletes it with LiveKit.

import (
	"fmt"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/rtc"
)

// OpusCap is the mic track a bot publishes on LiveKit: Opus as the app
// sends it.
var OpusCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1"}

// lkMic is a microphone on LiveKit: a sample track, each frame written
// with its length and level.
type lkMic struct {
	room  *lksdk.Room
	track *lksdk.LocalSampleTrack
}

func joinLiveKit(m rtc.Join, name string) (Mic, error) {
	room, err := lksdk.ConnectToRoomWithToken(m.URL, m.Token, lksdk.NewRoomCallback(), lksdk.WithAutoSubscribe(false))
	if err != nil {
		return nil, fmt.Errorf("LiveKit: %w", err)
	}
	track, err := lksdk.NewLocalSampleTrack(OpusCap)
	if err != nil {
		room.Disconnect()
		return nil, err
	}
	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: name, Source: livekit.TrackSource_MICROPHONE}); err != nil {
		room.Disconnect()
		return nil, err
	}
	return &lkMic{room: room, track: track}, nil
}

func (m *lkMic) WriteFrame(frame []byte, ts uint32, level *uint8) error {
	d := time.Duration(opus.PacketSamples(frame)) * time.Second / opus.SampleRate
	return m.track.WriteSample(media.Sample{Data: frame, Duration: d, PacketTimestamp: ts}, &lksdk.SampleWriteOptions{AudioLevel: level})
}

func (m *lkMic) Close() { m.room.Disconnect() }
