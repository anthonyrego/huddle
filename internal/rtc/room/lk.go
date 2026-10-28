package room

// LiveKit as a room's transport, through its Go SDK (lksdk, its root
// package only: pkg/media pulls libopus through cgo). It stays until
// huddle's own SFU matches its numbers (the media server's step 5).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
)

var logOnce sync.Once

// QuietSDKLogs sends lksdk's and pion's logs to slog's default handler,
// warnings and up (at info they log every ICE state change). Connect
// calls it; tools that use lksdk directly (the bots) call it first.
func QuietSDKLogs() { sdkLogs() }

func sdkLogs() {
	logOnce.Do(func() {
		h := slog.Default().Handler()
		lksdk.SetLogger(protoLogger.LogRLogger(logr.FromSlogHandler(levelAtLeast{h, slog.LevelWarn})))
	})
}

type levelAtLeast struct {
	slog.Handler
	min slog.Level
}

func (l levelAtLeast) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= l.min && l.Handler.Enabled(ctx, lv)
}

// lkTransport is a LiveKit room joined through lksdk.
type lkTransport struct {
	lk *lksdk.Room

	mu  sync.Mutex
	ccs map[string]*icept.Estimator // every peer connection's, by id
}

// dialLK joins a LiveKit room with the token j carries.
func dialLK(ctx context.Context, j rtc.Join, cfg dialConfig, ev transportEvents) (transport, error) {
	sdkLogs()
	t := &lkTransport{ccs: map[string]*icept.Estimator{}}
	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				t.subscribed(track, pub, rp, ev)
			},
			OnTrackUnsubscribed: func(_ *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				ev.unsubscribed(pub.SID())
			},
		},
		OnParticipantConnected:    func(p *lksdk.RemoteParticipant) { ev.arrived(lkParticipant(p)) },
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) { ev.left(p.Identity()) },
		OnActiveSpeakersChanged: func(ps []lksdk.Participant) {
			ls := map[string]float32{}
			for _, p := range ps {
				ls[p.Identity()] = max(ls[p.Identity()], p.AudioLevel())
			}
			ev.speakers(ls)
		},
		OnReconnecting:           ev.reconnecting,
		OnReconnected:            ev.reconnected,
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) { ev.disconnected(string(reason)) },
	}
	// lksdk puts one list of interceptors on both its connections, so the
	// room's two go on each, and congestion control after them, with the
	// transport-wide sequence numbers its feedback needs (lksdk doesn't add
	// them for what it sends); the publisher connection's estimator is the
	// one found by its id. lksdk's own interceptors (NACK, RTCP reports,
	// receive-side TWCC) come after ours.
	ccf, err := icept.NewCC(func(id string, e *icept.Estimator) {
		t.mu.Lock()
		t.ccs[id] = e
		t.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	twccHdr, err := twcc.NewHeaderExtensionInterceptor()
	if err != nil {
		return nil, err
	}
	ics := append(append(append([]interceptor.Factory{}, cfg.recv...), cfg.send...), ccf, twccHdr)
	opts := []lksdk.ConnectOption{lksdk.WithAutoSubscribe(cfg.subscribe),
		lksdk.WithInterceptors(ics), lksdk.WithIncludeDefaultInterceptors(true)}
	t.lk = lksdk.NewRoom(cb)
	if err := t.lk.JoinWithContextAndToken(ctx, j.URL, j.Token, opts...); err != nil {
		return nil, err
	}
	return t, nil
}

func lkParticipant(p *lksdk.RemoteParticipant) rtc.Participant {
	return rtc.Participant{Identity: p.Identity(), Name: p.Name(), Agent: p.Kind() == lksdk.ParticipantAgent}
}

func lkSource(s livekit.TrackSource) source {
	switch s {
	case livekit.TrackSource_MICROPHONE:
		return sourceMic
	case livekit.TrackSource_CAMERA:
		return sourceCamera
	case livekit.TrackSource_SCREEN_SHARE:
		return sourceScreen
	}
	return sourceOther
}

func (t *lkTransport) subscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant, ev transportEvents) {
	rt := remoteTrack{sid: pub.SID(), who: rp.Identity(), name: pub.Name(), kind: track.Kind(), mime: track.Codec().MimeType,
		source: lkSource(pub.Source()), track: track}
	if rt.kind == webrtc.RTPCodecTypeVideo {
		rt.video = &lkRemoteVideo{pub: pub, rp: rp, track: track}
	}
	for _, h := range pub.Receiver().GetParameters().HeaderExtensions {
		if h.URI == sdp.AudioLevelURI {
			rt.levelExt = h.ID
		}
	}
	if hook := ev.subscribed(rt); hook != nil {
		pub.OnRTCP(hook)
	}
}

func (t *lkTransport) self() rtc.Participant {
	lp := t.lk.LocalParticipant
	return rtc.Participant{Identity: lp.Identity(), Name: lp.Name()}
}

func (t *lkTransport) roomName() string { return t.lk.Name() }

func (t *lkTransport) participants() []rtc.Participant {
	var ps []rtc.Participant
	for _, p := range t.lk.GetRemoteParticipants() {
		ps = append(ps, lkParticipant(p))
	}
	return ps
}

func (t *lkTransport) estimator() *icept.Estimator {
	pc := t.lk.LocalParticipant.GetPublisherPeerConnection()
	if pc == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ccs[pc.ID()]
}

func (t *lkTransport) leave() { t.lk.Disconnect() }

// lkOpus is what the mic is sent as: Opus with in-band FEC and DTX.
var lkOpus = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeOpus, ClockRate: opus.SampleRate, Channels: 2,
	SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1",
}

// lkAudio is the mic as an lksdk sample track.
type lkAudio struct {
	track *lksdk.LocalSampleTrack
	pub   *lksdk.LocalTrackPublication
}

func (t *lkTransport) publishAudio() (audioOut, error) {
	track, err := lksdk.NewLocalSampleTrack(lkOpus)
	if err != nil {
		return nil, err
	}
	pub, err := t.lk.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "microphone", Source: livekit.TrackSource_MICROPHONE})
	if err != nil {
		return nil, err
	}
	return &lkAudio{track: track, pub: pub}, nil
}

func (a *lkAudio) sid() string { return a.pub.SID() }

// write sends a packet with its level on lksdk's audio-level extension
// (lksdk's LocalTrack can; pion's TrackLocalStaticSample can't).
func (a *lkAudio) write(p []byte, ts uint32, level uint8) error {
	return a.track.WriteSample(media.Sample{Data: p, Duration: 20 * time.Millisecond, PacketTimestamp: ts}, &lksdk.SampleWriteOptions{AudioLevel: &level})
}

func (a *lkAudio) setMuted(muted bool) { a.pub.SetMuted(muted) }

// lkH264 is what every video is sent as: H.264 Constrained Baseline, one
// NAL unit per packet or fragmented (packetization mode 1).
var lkH264 = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

// lkQualities are the layers' LiveKit qualities, by how many there are,
// largest first.
var lkQualities = map[int][]livekit.VideoQuality{
	1: {livekit.VideoQuality_HIGH},
	2: {livekit.VideoQuality_HIGH, livekit.VideoQuality_LOW},
	3: {livekit.VideoQuality_HIGH, livekit.VideoQuality_MEDIUM, livekit.VideoQuality_LOW},
}

// lkVideo is a video's layers as lksdk tracks, published together as one
// simulcast track.
type lkVideo struct {
	lp     *lksdk.LocalParticipant
	tracks []*lksdk.LocalTrack
	pub    *lksdk.LocalTrackPublication
}

func (t *lkTransport) publishVideo(kind rtc.VideoKind, layers []rtc.Layer, onRTCP func(layer int, p rtcp.Packet)) (videoOut, error) {
	qs := lkQualities[len(layers)]
	if qs == nil {
		return nil, fmt.Errorf("video: %d layers (1 to 3)", len(layers))
	}
	v := &lkVideo{lp: t.lk.LocalParticipant}
	id := fmt.Sprintf("huddle-%s-%08x", kind, rand.Uint32())
	for i, l := range layers {
		vl := &livekit.VideoLayer{Quality: qs[i], Width: uint32(l.W), Height: uint32(l.H), Bitrate: uint32(l.Bitrate)}
		tr, err := lksdk.NewLocalTrack(lkH264, lksdk.WithSimulcast(id, vl), lksdk.WithRTCPHandler(func(pkt rtcp.Packet) { onRTCP(i, pkt) }))
		if err != nil {
			return nil, err
		}
		v.tracks = append(v.tracks, tr)
	}
	source, name := livekit.TrackSource_CAMERA, "camera"
	if kind == rtc.Screen {
		source, name = livekit.TrackSource_SCREEN_SHARE, "screen"
	}
	pub, err := v.lp.PublishSimulcastTrack(v.tracks, &lksdk.TrackPublicationOptions{
		Name: name, Source: source, VideoWidth: layers[0].W, VideoHeight: layers[0].H})
	if err != nil {
		return nil, fmt.Errorf("publish the %s: %w", kind, err)
	}
	if pub == nil {
		return nil, errors.New("video: nothing published")
	}
	v.pub = pub
	return v, nil
}

func (v *lkVideo) sid() string { return v.pub.SID() }

func (v *lkVideo) write(layer int, au []byte, ts uint32) error {
	if ts == 0 {
		ts = 1 // lksdk reads 0 as "none given"
	}
	// The duration only sets the least step between timestamps (lksdk
	// would hold the clock back to it); the timestamp itself is the
	// capture time's.
	return v.tracks[layer].WriteSample(media.Sample{Data: au, Duration: time.Millisecond, PacketTimestamp: ts}, nil)
}

func (v *lkVideo) unpublish() error {
	if err := v.lp.UnpublishTrack(v.pub.SID()); err != nil && !errors.Is(err, lksdk.ErrCannotFindTrack) {
		return err
	}
	return nil
}

// lkRemoteVideo asks LiveKit for a video track's layer, and its
// publisher for keyframes.
type lkRemoteVideo struct {
	pub   *lksdk.RemoteTrackPublication
	rp    *lksdk.RemoteParticipant
	track *webrtc.TrackRemote
}

func (v *lkRemoteVideo) setSize(w, h int)   { v.pub.SetVideoDimensions(uint32(w), uint32(h)) }
func (v *lkRemoteVideo) setEnabled(on bool) { v.pub.SetEnabled(on) }

// pli goes through the subscriber connection (huddle doesn't use lksdk's
// single-connection mode, where WritePLI does nothing).
func (v *lkRemoteVideo) pli() { v.rp.WritePLI(webrtc.SSRC(v.track.SSRC())) }
