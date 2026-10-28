package fixturemeet

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"github.com/kazzmir/opus-go/ogg"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
)

// OpusCap is the mic track a bot publishes: Opus as the app sends it.
var OpusCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1"}

// Track is a mono Ogg/Opus file's packets, each with its RFC 6464 level
// (decoded once, when read), ready to play as a participant's mic.
type Track struct {
	pkts   [][]byte
	levels []uint8
}

// ReadTrack reads every audio packet of a mono Ogg/Opus file.
func ReadTrack(path string) (*Track, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := ogg.NewOpusReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Head.Channels != 1 {
		return nil, fmt.Errorf("%s has %d channels, want 1", path, r.Head.Channels)
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	t := &Track{}
	out := make([]float32, 6*opus.FrameSamples)
	for {
		p, err := r.ReadAudioPacket()
		if err != nil {
			break
		}
		n, err := dec.Decode(p.Data, out)
		if err != nil || n == 0 {
			continue
		}
		t.pkts = append(t.pkts, p.Data)
		t.levels = append(t.levels, uint8(min(max(-pcm.LevelF32(out[:n]), 0), 127)))
	}
	if len(t.pkts) == 0 {
		return nil, fmt.Errorf("%s has no Opus packets", path)
	}
	return t, nil
}

// Packets is how many packets the track holds.
func (t *Track) Packets() int { return len(t.pkts) }

// Play writes the track to lt in real time from t0, each packet with its
// length, RTP timestamp and level, until it ends (or, with loop, forever)
// or ctx does.
func (t *Track) Play(ctx context.Context, lt *lksdk.LocalSampleTrack, t0 time.Time, loop bool) error {
	ts := rand.Uint32() | 1
	var elapsed time.Duration
	for i := 0; ; i++ {
		if i == len(t.pkts) {
			if !loop {
				return nil
			}
			i = 0
		}
		n := opus.PacketSamples(t.pkts[i])
		d := time.Duration(n) * time.Second / opus.SampleRate
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(t0.Add(elapsed))):
		}
		lvl := t.levels[i]
		if err := lt.WriteSample(media.Sample{Data: t.pkts[i], Duration: d, PacketTimestamp: ts}, &lksdk.SampleWriteOptions{AudioLevel: &lvl}); err != nil {
			return err
		}
		ts += uint32(n)
		elapsed += d
	}
}
