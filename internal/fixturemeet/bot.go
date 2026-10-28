package fixturemeet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/wire"
)

// Join asks a dev server to let name into room.
func Join(ctx context.Context, server, room, name string) (wire.Joined, error) {
	var j wire.Joined
	body, _ := json.Marshal(wire.DevJoin{Room: room, Name: name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/dev/join", bytes.NewReader(body))
	if err != nil {
		return j, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return j, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return j, fmt.Errorf("join: %s: %s", res.Status, b)
	}
	return j, json.NewDecoder(res.Body).Decode(&j)
}

// Bot is a fixture participant: a name and the track it plays.
type Bot struct {
	Name string
	Ogg  string

	room  *lksdk.Room
	track *lksdk.LocalTrack
	r     *oggreader.OggReader
	f     *os.File
}

// Connect joins the bot to the room through the dev server and publishes
// its (still quiet) track.
func (b *Bot) Connect(ctx context.Context, server, room string) error {
	f, err := os.Open(b.Ogg)
	if err != nil {
		return fmt.Errorf("%w (make fixtures renders it)", err)
	}
	r, _, err := oggreader.NewOggReader(f)
	if err != nil {
		f.Close()
		return err
	}
	b.f, b.r = f, r
	j, err := Join(ctx, server, room, b.Name)
	if err != nil {
		return err
	}
	b.room, err = lksdk.ConnectToRoomWithToken(j.LiveKitURL, j.Token, lksdk.NewRoomCallback(), lksdk.WithAutoSubscribe(false))
	if err != nil {
		return fmt.Errorf("%s: LiveKit: %w", b.Name, err)
	}
	b.track, err = lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err != nil {
		return err
	}
	_, err = b.room.LocalParticipant.PublishTrack(b.track, &lksdk.TrackPublicationOptions{Name: "microphone"})
	return err
}

// Play sends the track in real time from t0, a packet every 20 ms, until
// it ends or ctx does.
func (b *Bot) Play(ctx context.Context, t0 time.Time) error {
	const frame = 20 * time.Millisecond
	next := t0
	for i := 0; ; i++ {
		pkt, err := b.r.ReadPacket()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(next)):
		}
		if err := b.track.WriteSample(media.Sample{Data: pkt, Duration: frame}, nil); err != nil {
			return err
		}
		next = next.Add(frame)
	}
}

// Close leaves the room.
func (b *Bot) Close() {
	if b.room != nil {
		b.room.Disconnect()
	}
	if b.f != nil {
		b.f.Close()
	}
}

// PlayAll joins every bot, then plays them together from a moment just
// after the last has joined, which it reports with started.
func PlayAll(ctx context.Context, server, room string, bots []*Bot, started func(t0 time.Time)) error {
	for _, b := range bots {
		if err := b.Connect(ctx, server, room); err != nil {
			for _, o := range bots {
				o.Close()
			}
			return err
		}
	}
	defer func() {
		for _, b := range bots {
			b.Close()
		}
	}()
	// Let subscriptions settle before the first word.
	t0 := time.Now().Add(1500 * time.Millisecond)
	if started != nil {
		started(t0)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(bots))
	for i, b := range bots {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = b.Play(ctx, t0) }()
	}
	wg.Wait()
	return errors.Join(errs...)
}
