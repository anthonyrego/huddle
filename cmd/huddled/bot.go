package main

// bot: dev participants that join a room straight through LiveKit (with
// the server's LiveKit key, as make bot passes it):
//
//   - echo repeats each person's voice 1.5 s later, as its own track, so
//     one person can test alone: you hear yourself once, and never an echo
//     of the echo if echo cancellation works (its own tracks and other
//     bots' aren't repeated).
//   - play is a fixture participant: it plays an Ogg/Opus file, looping.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kazzmir/opus-go/ogg"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/rtc/lkrtc"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/lktoken"
)

func init() {
	register(&command{name: "bot", summary: "dev participants: bot echo (repeats each voice 1.5 s later), bot play -ogg f.ogg", run: runBot})
}

// botSuffix ends every bot's identity, so bots don't echo bots.
const botSuffix = "-bot"

var opusCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1"}

func runBot(args []string) error {
	if len(args) == 0 || (args[0] != "echo" && args[0] != "play") {
		return errors.New("usage: huddled bot echo [-room R] [-delay 1.5s] | huddled bot play -ogg f.ogg [-name N] [-room R] [-loop=false]")
	}
	kind := args[0]
	fs := flag.NewFlagSet("bot "+kind, flag.ContinueOnError)
	room := fs.String("room", "test", "the room")
	name := fs.String("name", "", "the name shown (default Echo, or the file's name)")
	delay := fs.Duration("delay", 1500*time.Millisecond, "echo: how much later each voice comes back")
	oggPath := fs.String("ogg", "", "play: the Ogg/Opus file (scripts/voice-fixtures.sh makes some)")
	loop := fs.Bool("loop", true, "play: start again at the end")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	lkrtc.QuietSDKLogs()
	if *name == "" {
		*name = "Echo"
		if kind == "play" {
			base := strings.TrimSuffix(filepath.Base(*oggPath), filepath.Ext(*oggPath))
			*name = strings.ToUpper(base[:min(1, len(base))]) + base[min(1, len(base)):]
		}
	}
	url := os.Getenv("LIVEKIT_URL")
	minter := lktoken.Minter{Key: os.Getenv("LIVEKIT_API_KEY"), Secret: os.Getenv("LIVEKIT_API_SECRET")}
	if url == "" || minter.Key == "" || minter.Secret == "" {
		return errors.New("set LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET (make bot does)")
	}
	roomName := httpapi.Slug(*room)
	identity := httpapi.Slug(*name) + botSuffix
	tok, err := minter.Mint(lktoken.Grant{Room: roomName, Identity: identity, Name: *name})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch kind {
	case "echo":
		return runEcho(ctx, url, tok, *delay)
	default:
		if *oggPath == "" {
			return errors.New("bot play needs -ogg")
		}
		return runPlay(ctx, url, tok, *oggPath, *loop)
	}
}

// echo is the echo bot: one delayed copy of each person's audio track.
type echo struct {
	lk    *lksdk.Room
	delay time.Duration
	mu    sync.Mutex
	pubs  map[string]string // remote track SID → our track SID
}

func runEcho(ctx context.Context, url, tok string, delay time.Duration) error {
	e := &echo{delay: delay, pubs: map[string]string{}}
	cb := &lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
		OnTrackSubscribed:   e.subscribed,
		OnTrackUnsubscribed: e.unsubscribed,
	}}
	e.lk = lksdk.NewRoom(cb)
	if err := e.lk.JoinWithToken(url, tok, lksdk.WithAutoSubscribe(true)); err != nil {
		return err
	}
	defer e.lk.Disconnect()
	slog.Info("bot: echo joined", "room", e.lk.Name(), "delay", delay)
	<-ctx.Done()
	return nil
}

func (e *echo) subscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() != webrtc.RTPCodecTypeAudio || strings.HasSuffix(rp.Identity(), botSuffix) {
		return
	}
	if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
		return
	}
	lt, err := lksdk.NewLocalTrack(opusCap)
	if err != nil {
		slog.Error("bot: echo track", "err", err)
		return
	}
	mine, err := e.lk.LocalParticipant.PublishTrack(lt, &lksdk.TrackPublicationOptions{Name: "echo of " + rp.Identity(), Source: livekit.TrackSource_MICROPHONE})
	if err != nil {
		slog.Error("bot: echo publish", "for", rp.Identity(), "err", err)
		return
	}
	e.mu.Lock()
	e.pubs[pub.SID()] = mine.SID()
	e.mu.Unlock()
	slog.Info("bot: echoing", "identity", rp.Identity(), "track", pub.SID(), "as", mine.SID())

	type held struct {
		p  *rtp.Packet
		at time.Time
	}
	q := make(chan held, 4096) // 80 s of 20 ms packets
	go func() {
		defer close(q)
		for {
			p, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			// The extensions were numbered for our subscription; the track
			// we publish negotiates its own.
			p.Extension, p.Extensions = false, nil
			select {
			case q <- held{p, time.Now().Add(e.delay)}:
			default:
			}
		}
	}()
	go func() {
		for h := range q {
			if d := time.Until(h.at); d > 0 {
				time.Sleep(d)
			}
			lt.WriteRTP(h.p, nil)
		}
	}()
}

func (e *echo) unsubscribed(_ *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	e.mu.Lock()
	mine, ok := e.pubs[pub.SID()]
	delete(e.pubs, pub.SID())
	e.mu.Unlock()
	if ok {
		e.lk.LocalParticipant.UnpublishTrack(mine)
		slog.Info("bot: stopped echoing", "identity", rp.Identity())
	}
}

// runPlay plays an Ogg/Opus file as a participant's mic, in real time,
// with each packet's RFC 6464 level.
func runPlay(ctx context.Context, url, tok, path string, loop bool) error {
	pkts, levels, err := readOggOpus(path)
	if err != nil {
		return err
	}
	lk := lksdk.NewRoom(&lksdk.RoomCallback{})
	if err := lk.JoinWithToken(url, tok, lksdk.WithAutoSubscribe(false)); err != nil {
		return err
	}
	defer lk.Disconnect()
	lt, err := lksdk.NewLocalSampleTrack(opusCap)
	if err != nil {
		return err
	}
	if _, err := lk.LocalParticipant.PublishTrack(lt, &lksdk.TrackPublicationOptions{Name: "microphone", Source: livekit.TrackSource_MICROPHONE}); err != nil {
		return err
	}
	slog.Info("bot: playing", "room", lk.Name(), "file", path, "packets", len(pkts), "loop", loop)
	ts := rand.Uint32() | 1
	start, elapsed := time.Now(), time.Duration(0)
	for i := 0; ; i++ {
		if i == len(pkts) {
			if !loop {
				return nil
			}
			i = 0
		}
		n := opus.PacketSamples(pkts[i])
		if n == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(start.Add(elapsed))):
		}
		lvl := levels[i]
		lt.WriteSample(media.Sample{Data: pkts[i], Duration: time.Duration(n) * time.Second / 48000, PacketTimestamp: ts}, &lksdk.SampleWriteOptions{AudioLevel: &lvl})
		ts += uint32(n)
		elapsed += time.Duration(n) * time.Second / 48000
	}
}

// readOggOpus reads every audio packet of a mono Ogg/Opus file, with each
// one's RFC 6464 level (decoded once, here).
func readOggOpus(path string) ([][]byte, []uint8, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	r, err := ogg.NewOpusReader(bytes.NewReader(b))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Head.Channels != 1 {
		return nil, nil, fmt.Errorf("%s has %d channels, want 1", path, r.Head.Channels)
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		return nil, nil, err
	}
	defer dec.Close()
	var pkts [][]byte
	var levels []uint8
	out := make([]float32, 6*opus.FrameSamples)
	for {
		p, err := r.ReadAudioPacket()
		if err != nil {
			break
		}
		n, err := dec.Decode(p.Data, out)
		if err != nil {
			continue
		}
		pkts = append(pkts, p.Data)
		levels = append(levels, uint8(min(max(-pcm.LevelF32(out[:n]), 0), 127)))
	}
	if len(pkts) == 0 {
		return nil, nil, fmt.Errorf("%s has no Opus packets", path)
	}
	return pkts, levels, nil
}
