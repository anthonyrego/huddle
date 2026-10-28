package main

// The echo bot on LiveKit, until the media server's step 5 deletes it with
// LiveKit.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/fixturemeet"
)

// lkEcho is the echo bot on LiveKit: one delayed copy of each person's
// audio track.
type lkEcho struct {
	lk    *lksdk.Room
	delay time.Duration
	mu    sync.Mutex
	pubs  map[string]string // remote track SID → our track SID
}

func runEchoLiveKit(ctx context.Context, url, tok string, delay time.Duration) error {
	e := &lkEcho{delay: delay, pubs: map[string]string{}}
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

func (e *lkEcho) subscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() != webrtc.RTPCodecTypeAudio || !echoes(rp.Identity(), rp.Kind() == lksdk.ParticipantAgent) {
		return
	}
	if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
		return
	}
	lt, err := lksdk.NewLocalTrack(fixturemeet.OpusCap)
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

func (e *lkEcho) unsubscribed(_ *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	e.mu.Lock()
	mine, ok := e.pubs[pub.SID()]
	delete(e.pubs, pub.SID())
	e.mu.Unlock()
	if ok {
		e.lk.LocalParticipant.UnpublishTrack(mine)
		slog.Info("bot: stopped echoing", "identity", rp.Identity())
	}
}
