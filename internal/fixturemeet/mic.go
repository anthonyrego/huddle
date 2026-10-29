package fixturemeet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// connectWait is how long a microphone is given to connect.
const connectWait = 10 * time.Second

// Mic is a voice published in a room's media: frames in (Track.Play writes
// them), and Close leaves the room.
type Mic interface {
	FrameWriter
	Close()
}

// JoinMic joins the media server a dev join named (devjoin.RTC's answer)
// and publishes a microphone named name there, returning once the
// microphone is connected, so the first frame written is heard. A bot that
// publishes doesn't subscribe to anything.
func JoinMic(ctx context.Context, m rtc.Join, name string) (Mic, error) {
	return joinSFU(ctx, m, name, sfuclient.Options{SendOnly: true})
}

// sfuMic is a microphone on the SFU: a session that only publishes.
type sfuMic struct {
	s    *sfuclient.Session
	t    *sfuclient.Track
	gone atomic.Pointer[error] // why the session ended
}

func joinSFU(ctx context.Context, m rtc.Join, name string, o sfuclient.Options) (Mic, error) {
	mic := &sfuMic{}
	up := make(chan struct{})
	var once sync.Once
	s, err := sfuclient.Join(ctx, m.URL, m.Token, o, sfuclient.Events{
		Connection: func(pc signal.PC, st webrtc.PeerConnectionState) {
			if pc == signal.Pub && st == webrtc.PeerConnectionStateConnected {
				once.Do(func() { close(up) })
			}
		},
		Disconnected: func(err error) { mic.gone.Store(&err) },
	})
	if err != nil {
		return nil, err
	}
	t, _, err := s.PublishAudio(ctx, name)
	if err != nil {
		s.Close()
		return nil, err
	}
	select {
	case <-up:
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	case <-time.After(connectWait):
		s.Close()
		return nil, fmt.Errorf("the microphone didn't connect in %v", connectWait)
	}
	mic.s, mic.t = s, t
	return mic, nil
}

// WriteFrame sends a frame. One the track isn't negotiated for is dropped,
// as a mic drops what it hears before it connects; a session that has
// ended is an error.
func (m *sfuMic) WriteFrame(frame []byte, ts uint32, level *uint8) error {
	if err := m.gone.Load(); err != nil {
		return fmt.Errorf("the media server: %w", *err)
	}
	if err := m.t.WriteFrame(frame, ts, level); err != nil && !errors.Is(err, sfuclient.ErrNotBound) {
		return err
	}
	return nil
}

func (m *sfuMic) Close() { m.s.Close() }
