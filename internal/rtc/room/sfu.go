package room

// huddle's own SFU (internal/server/sfu) as a room's transport, through
// sfuclient: a join of the server's /rtc with the token the dev join gave,
// a connection to publish on and one to receive on. It mirrors lk.go; what
// LiveKit's SDK did for us there (participants, subscriptions, simulcast,
// the audio level) sfuclient and the SFU do here.
//
// sfuclient has no resume: a connection that fails, the signalling going,
// or the server's bye ends the session (disconnected), and the app joins
// again (call.go rejoins on Disconnected). So reconnecting and reconnected
// never fire on this transport.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/rtc/sfuclient"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// sfuTransport is a room of huddle's SFU, joined through sfuclient.
type sfuTransport struct {
	s      *sfuclient.Session
	ctx    context.Context // ends at leave: bounds publishing
	cancel context.CancelFunc
	est    atomic.Pointer[icept.Estimator] // the publish connection's
	// ready closes once Join has returned: events (which can come before
	// it does) wait for it, so whatever they hand the room has a session.
	ready chan struct{}

	// What we publish of video, by SID, for the server's wanted; a wanted
	// for a video not yet in videos (Publish hasn't returned) waits in
	// wanted.
	mu     sync.Mutex
	videos map[string]*sfuVideo
	wanted map[string][]string
}

// dialSFU joins the SFU's room with the token j carries.
func dialSFU(ctx context.Context, j rtc.Join, cfg dialConfig, ev transportEvents) (transport, error) {
	t := &sfuTransport{ready: make(chan struct{}), videos: map[string]*sfuVideo{}, wanted: map[string][]string{}}
	defer close(t.ready)
	// Congestion control goes on the publish connection after the room's
	// own (the test uplink), and sfuclient makes that connection once,
	// inside Join: the estimator comes then. sfuclient numbers the packets
	// for transport-wide feedback itself; adding the numbers again would
	// give GCC every packet twice.
	ccf, err := icept.NewCC(func(_ string, e *icept.Estimator) { t.est.Store(e) })
	if err != nil {
		return nil, err
	}
	opts := sfuclient.Options{
		Publish:   append(slices.Clone(cfg.send), ccf),
		Subscribe: slices.Clone(cfg.recv),
		// The receive side's simulation drops after SRTP; the SFU's
		// resends would read as replays (sfuclient.Options).
		NoReplayProtection: len(cfg.recv) > 0,
		SendOnly:           !cfg.subscribe,
		Loopback:           loopbackURL(j.URL),
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	s, err := sfuclient.Join(ctx, j.URL, j.Token, opts, t.events(ev))
	if err != nil {
		t.cancel()
		return nil, sfuJoinError(err)
	}
	t.s = s
	return t, nil
}

// loopbackURL says whether a media server's URL is on this machine; then
// the connections offer loopback candidates too (a server whose node IP is
// 127.0.0.1 offers only those).
func loopbackURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := p.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// sfuJoinError says why a join failed in the words the app shows ("Voice:
// …" in the device menu), as LiveKit's do: a server that can't be reached,
// or one that turned the join away.
func sfuJoinError(err error) error {
	var op *net.OpError
	var refused *sfuclient.RefusedError
	switch {
	case errors.As(err, &refused):
		return fmt.Errorf("the media server refused the join: %s", refused.Reason)
	case errors.As(err, &op):
		return fmt.Errorf("could not reach the media server: %w", op)
	}
	return err
}

// events turns the session's events into the room's. They come in order
// on the session's goroutine.
func (t *sfuTransport) events(ev transportEvents) sfuclient.Events {
	wait := func() { <-t.ready }
	return sfuclient.Events{
		Arrived: func(p signal.Participant) {
			wait()
			ev.arrived(sfuParticipant(p))
		},
		Left: func(identity string) {
			wait()
			ev.left(identity)
		},
		TrackAdded: func(rt *sfuclient.RemoteTrack) {
			wait()
			t.subscribed(rt, ev)
		},
		TrackRemoved: func(rt *sfuclient.RemoteTrack) {
			wait()
			ev.unsubscribed(rt.SID)
		},
		Speakers: func(sp []signal.Speaker) {
			wait()
			ls := make(map[string]float32, len(sp))
			for _, s := range sp {
				ls[s.Identity] = max(ls[s.Identity], s.Level)
			}
			ev.speakers(ls)
		},
		Wanted: func(w signal.Wanted) {
			wait()
			t.mu.Lock()
			v := t.videos[w.SID]
			if v == nil {
				t.wanted[w.SID] = w.RIDs
			}
			t.mu.Unlock()
			if v != nil {
				v.setWanted(w.RIDs)
			}
		},
		Disconnected: func(err error) {
			wait()
			ev.disconnected(err.Error())
		},
	}
}

func sfuParticipant(p signal.Participant) rtc.Participant {
	return rtc.Participant{Identity: p.Identity, Name: p.Name, Agent: p.Agent}
}

func sfuSource(s signal.Source) source {
	switch s {
	case signal.Microphone:
		return sourceMic
	case signal.Camera:
		return sourceCamera
	case signal.Screen:
		return sourceScreen
	}
	return sourceOther
}

// subscribed hands the room a track we're sent, and reads its RTCP (the
// SFU's sender reports, for lip sync) for whatever the room returns to
// hear it. The RTCP is read even when nothing hears it: pion's receiver
// reports are made from the sender reports as they're read.
func (t *sfuTransport) subscribed(rt *sfuclient.RemoteTrack, ev transportEvents) {
	params := rt.Receiver.GetParameters()
	r := remoteTrack{sid: rt.SID, who: rt.Participant, name: rt.Name, kind: rt.Track.Kind(), source: sfuSource(rt.Source), track: rt.Track}
	if len(params.Codecs) > 0 {
		r.mime = params.Codecs[0].MimeType
	}
	for _, h := range params.HeaderExtensions {
		if h.URI == sdp.AudioLevelURI {
			r.levelExt = h.ID
		}
	}
	if r.kind == webrtc.RTPCodecTypeVideo {
		r.video = &sfuRemoteVideo{s: t.s, sid: rt.SID, ssrc: rt.Track.SSRC(), on: true}
	}
	hook := ev.subscribed(r)
	go func() {
		for {
			pkts, _, err := rt.Receiver.ReadRTCP()
			if err != nil {
				return
			}
			if hook != nil {
				for _, p := range pkts {
					hook(p)
				}
			}
		}
	}()
}

func (t *sfuTransport) self() rtc.Participant { return sfuParticipant(t.s.Self()) }

func (t *sfuTransport) roomName() string { return t.s.Room() }

func (t *sfuTransport) participants() []rtc.Participant {
	var ps []rtc.Participant
	for _, p := range t.s.Participants() {
		ps = append(ps, sfuParticipant(p))
	}
	return ps
}

func (t *sfuTransport) estimator() *icept.Estimator { return t.est.Load() }

func (t *sfuTransport) leave() {
	t.cancel()
	t.s.Close()
}

// sfuAudio is the mic as an sfuclient track.
type sfuAudio struct {
	s     *sfuclient.Session
	track *sfuclient.Track
	id    string
}

func (t *sfuTransport) publishAudio() (audioOut, error) {
	track, sid, err := t.s.PublishAudio(t.ctx, "microphone")
	if err != nil {
		return nil, err
	}
	return &sfuAudio{s: t.s, track: track, id: sid}, nil
}

func (a *sfuAudio) sid() string { return a.id }

// write sends a packet with its level; one written before the track is
// negotiated is dropped (sfuclient.ErrNotBound).
func (a *sfuAudio) write(p []byte, ts uint32, level uint8) error {
	return a.track.WriteFrame(p, ts, &level)
}

func (a *sfuAudio) setMuted(muted bool) {
	if err := a.s.Mute(a.id, muted); err != nil {
		slog.Debug("voice: mute", "err", err)
	}
}

// sfuRIDs are the layers' simulcast RIDs, by how many there are, largest
// first (internal/wire/signal).
var sfuRIDs = map[int][]string{
	1: {"f"},
	2: {"f", "q"},
	3: {"f", "h", "q"},
}

// sfuVideo is a video's layers as sfuclient tracks, published together as
// one simulcast track.
type sfuVideo struct {
	t      *sfuTransport
	s      *sfuclient.Session
	id     string
	tracks []*sfuclient.Track // by layer, largest first
	rids   []string           // by layer

	mu     sync.Mutex
	wanted []bool // the server's last word (nil: none yet, every layer wanted)
	hook   func([]bool)
}

func (t *sfuTransport) publishVideo(kind rtc.VideoKind, layers []rtc.Layer, onRTCP func(layer int, p rtcp.Packet)) (videoOut, error) {
	rids := sfuRIDs[len(layers)]
	if rids == nil {
		return nil, fmt.Errorf("video: %d layers (1 to 3)", len(layers))
	}
	cid := fmt.Sprintf("huddle-%s-%08x", kind, rand.Uint32())
	req := signal.Publish{CID: cid, Kind: signal.Video, Source: signal.Camera, Name: "camera"}
	if kind == rtc.Screen {
		req.Source, req.Name = signal.Screen, "screen"
	}
	v := &sfuVideo{t: t, s: t.s, tracks: make([]*sfuclient.Track, len(layers)), rids: rids}
	for i, l := range layers {
		req.Layers = append(req.Layers, signal.Layer{RID: rids[i], W: l.W, H: l.H, Bitrate: l.Bitrate})
		tr, err := sfuclient.NewTrack(codecs.H264, cid, t.s.Self().Identity, rids[i], func(p rtcp.Packet) { onRTCP(i, p) })
		if err != nil {
			return nil, err
		}
		v.tracks[i] = tr
	}
	// The smallest layer is the transceiver's and the larger its further
	// encodings, as the SFU's tests publish them.
	order := slices.Clone(v.tracks)
	slices.Reverse(order)
	sid, err := t.s.Publish(t.ctx, req, order...)
	if err != nil {
		return nil, fmt.Errorf("publish the %s: %w", kind, err)
	}
	v.id = sid
	t.mu.Lock()
	t.videos[sid] = v
	rids, heard := t.wanted[sid]
	delete(t.wanted, sid)
	t.mu.Unlock()
	if heard {
		v.setWanted(rids)
	}
	return v, nil
}

func (v *sfuVideo) sid() string { return v.id }

// setWanted takes the server's wanted (by RID) for the video, as a bool per
// layer.
func (v *sfuVideo) setWanted(rids []string) {
	w := make([]bool, len(v.rids))
	for i, rid := range v.rids {
		w[i] = slices.Contains(rids, rid)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.wanted = w
	if v.hook != nil {
		v.hook(w)
	}
}

// onWanted is wantedOut's. The hook is called under v.mu, so it hears the
// server's words in order.
func (v *sfuVideo) onWanted(f func([]bool)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.hook = f
	if v.wanted != nil {
		f(v.wanted)
	}
}

// write sends one access unit of a layer; one written before the layer is
// negotiated is dropped (sfuclient.ErrNotBound).
func (v *sfuVideo) write(layer int, au []byte, ts uint32) error {
	return v.tracks[layer].WriteFrame(au, ts, nil)
}

func (v *sfuVideo) unpublish() error {
	v.t.mu.Lock()
	delete(v.t.videos, v.id)
	v.t.mu.Unlock()
	v.mu.Lock()
	v.hook = nil
	v.mu.Unlock()
	return v.s.Unpublish(v.id)
}

// sfuRemoteVideo asks the SFU for a video track's layer (signal.Settings:
// a size, and whether it's on), and its publisher for keyframes.
type sfuRemoteVideo struct {
	s    *sfuclient.Session
	sid  string
	ssrc webrtc.SSRC

	mu   sync.Mutex
	w, h int
	on   bool
}

func (v *sfuRemoteVideo) setSize(w, h int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.w, v.h = w, h
	v.send()
}

func (v *sfuRemoteVideo) setEnabled(on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.on = on
	v.send()
}

// send tells the SFU the settings now; v.mu is held, so the last change is
// the last sent.
func (v *sfuRemoteVideo) send() {
	if err := v.s.Settings(signal.Settings{SID: v.sid, W: v.w, H: v.h, Enabled: v.on}); err != nil {
		slog.Debug("video: settings", "track", v.sid, "err", err)
	}
}

// pli goes through the receive connection, for the track's SSRC there.
func (v *sfuRemoteVideo) pli() {
	if err := v.s.PLI(v.ssrc); err != nil {
		slog.Debug("video: pli", "track", v.sid, "err", err)
	}
}
