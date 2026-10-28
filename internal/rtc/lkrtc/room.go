// Package lkrtc is the rtc interface on LiveKit's Go SDK (lksdk, its root
// package only: pkg/media pulls libopus through cgo). It publishes the mic
// as Opus through an lksdk sample track, with the RFC 6464 audio level on
// every packet (the SFU's speaker detection reads it), subscribes to every
// remote audio track as raw RTP, gives each its own jitter buffer, and
// mixes them for the speaker with the playout package.
package lkrtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/jitter"
	"github.com/anthonyrego/huddle/internal/netsim"
	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/playout"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/video"
)

// Connector joins LiveKit rooms. The zero value is the voice's defaults.
type Connector struct {
	// Sim, when set, is called once per remote stream; an active Sim makes
	// that stream's network worse on receive, before the jitter buffer.
	Sim func() *netsim.Sim
	// Jitter tunes the jitter buffers; Opus the encoder.
	Jitter jitter.Config
	Opus   opus.Options
	// StatsEvery is how often each track's numbers go to the log; 0 is
	// 10 s, negative never.
	StatsEvery time.Duration
	// Arrival, when set, is told of every received audio packet (a debug
	// trace: huddle voice -arrivals).
	Arrival func(identity string, seq uint16, ts uint32, size int, at time.Time)
	// NewDecoder makes a decoder for each remote video track (the app's
	// platform one); without it, video tracks aren't played.
	NewDecoder func() (rtc.VideoDecoder, error)
	// Uplink, when set, narrows what this side sends to that many bits a
	// second, with a 50-packet queue in front (a test of congestion
	// control: huddle video -uplink).
	Uplink int
	// SendOnly subscribes to nothing (bots that only publish).
	SendOnly bool
	// VideoFrame, when set, is told of every remote video frame as it is
	// shown, on the track's goroutine (a measuring hook: huddle video).
	// It must not keep the frame.
	VideoFrame func(t rtc.VideoTrack, f video.Frame, info VideoFrameInfo)
}

// NewSim is a Connector.Sim with fixed settings and a fresh seed per stream.
func NewSim(loss float64, jitterAmount time.Duration, reorder float64) func() *netsim.Sim {
	return func() *netsim.Sim { return netsim.New(loss, jitterAmount, reorder, simSeed()) }
}

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

// Connect joins the room the token is for.
func (c *Connector) Connect(ctx context.Context, j rtc.Join, ev rtc.Events) (rtc.Room, error) {
	sdkLogs()
	r := &room{cfg: c, ev: ev, mix: playout.New(), tracks: map[string]*remote{}, videos: map[string]*videoTrack{}, sfuLevel: map[string]float32{}, quit: make(chan struct{})}
	r.cc = newVideoCC(r)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed:   r.subscribed,
			OnTrackUnsubscribed: r.unsubscribed,
		},
		OnParticipantConnected: func(p *lksdk.RemoteParticipant) {
			slog.Info("voice: arrived", "identity", p.Identity())
			if ev.Arrived != nil {
				ev.Arrived(participant(p))
			}
		},
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) {
			slog.Info("voice: left", "identity", p.Identity())
			if ev.Left != nil {
				ev.Left(p.Identity())
			}
		},
		// The SFU's speaker detection runs on the RFC 6464 levels we send;
		// what it reports is kept for the log, as proof they arrive.
		OnActiveSpeakersChanged: func(ps []lksdk.Participant) {
			r.mu.Lock()
			for _, p := range ps {
				r.sfuLevel[p.Identity()] = max(r.sfuLevel[p.Identity()], p.AudioLevel())
			}
			r.mu.Unlock()
		},
		OnReconnecting: func() {
			slog.Warn("voice: reconnecting")
			if ev.Reconnecting != nil {
				ev.Reconnecting()
			}
		},
		OnReconnected: func() {
			slog.Info("voice: reconnected")
			if ev.Reconnected != nil {
				ev.Reconnected()
			}
		},
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			if r.left.Load() {
				return
			}
			slog.Warn("voice: disconnected", "reason", reason)
			if ev.Disconnected != nil {
				ev.Disconnected(fmt.Errorf("disconnected: %v", reason))
			}
		},
	}
	// Our interceptors, nearest the network first: the receive side's
	// simulated network; the test uplink; congestion control (GCC and the
	// pacer) and the transport-wide sequence numbers its feedback needs,
	// which lksdk doesn't add for what it sends. lksdk's own (NACK, RTCP
	// reports, receive-side TWCC) come after.
	var ics []interceptor.Factory
	if c.Sim != nil {
		ics = append(ics, simFactory{c.Sim})
	}
	if c.Uplink > 0 {
		ics = append(ics, &uplinkFactory{bps: c.Uplink, slots: uplinkSlots, made: r.cc.addUplink})
	}
	ccf, err := newCC(r.cc.found)
	if err != nil {
		r.cancel()
		return nil, err
	}
	twccHdr, err := twcc.NewHeaderExtensionInterceptor()
	if err != nil {
		r.cancel()
		return nil, err
	}
	ics = append(ics, ccf, twccHdr)
	opts := []lksdk.ConnectOption{lksdk.WithAutoSubscribe(!c.SendOnly),
		lksdk.WithInterceptors(ics), lksdk.WithIncludeDefaultInterceptors(true)}
	r.lk = lksdk.NewRoom(cb)
	if err := r.lk.JoinWithContextAndToken(ctx, j.URL, j.Token, opts...); err != nil {
		r.cancel()
		return nil, fmt.Errorf("join the voice room: %w", err)
	}
	lp := r.lk.LocalParticipant
	r.self = rtc.Participant{Identity: lp.Identity(), Name: lp.Name()}
	others := r.Participants()
	slog.Info("voice: joined", "room", r.lk.Name(), "identity", r.self.Identity, "others", len(others))
	if ev.Joined != nil {
		ev.Joined(r.self, others)
	}
	go r.levels()
	if every := c.statsEvery(); every > 0 {
		go r.logStats(every)
	}
	return r, nil
}

func (c *Connector) statsEvery() time.Duration {
	if c.StatsEvery == 0 {
		return 10 * time.Second
	}
	return c.StatsEvery
}

func participant(p *lksdk.RemoteParticipant) rtc.Participant {
	return rtc.Participant{Identity: p.Identity(), Name: p.Name(), Agent: p.Kind() == lksdk.ParticipantAgent}
}

type room struct {
	cfg    *Connector
	ev     rtc.Events
	lk     *lksdk.Room
	self   rtc.Participant
	mix    *playout.Mixer
	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}
	left   atomic.Bool
	once   sync.Once

	cc *videoCC

	mu       sync.Mutex
	tracks   map[string]*remote     // by track SID
	videos   map[string]*videoTrack // by track SID
	spk      rtc.Speaker
	pub      *lksdk.LocalTrackPublication
	sfuLevel map[string]float32 // the loudest the SFU has said each speaker was, 0–1

	muted     atomic.Bool
	selfLevel atomic.Int64 // the mic's level, dBFS × 100
	playing   atomic.Bool
}

// remote is one subscribed audio track.
type remote struct {
	sid, who string
	arrival  func(identity string, seq uint16, ts uint32, size int, at time.Time)
	buf      *jitter.Buffer
	levelExt atomic.Int32 // the last RFC 6464 level the packets carried, -1 none
	sr       srClock      // its sender reports, for lip sync
}

func (r *room) subscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		r.subscribedVideo(track, pub, rp)
		return
	}
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
		slog.Warn("voice: not Opus, not played", "identity", rp.Identity(), "codec", track.Codec().MimeType)
		return
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		slog.Error("voice: decoder", "err", err)
		return
	}
	t := &remote{sid: pub.SID(), who: rp.Identity(), buf: jitter.New(dec, r.cfg.Jitter), arrival: r.cfg.Arrival}
	t.levelExt.Store(-1)
	pub.OnRTCP(t.sr.read)
	r.mu.Lock()
	r.tracks[t.sid] = t
	r.mu.Unlock()
	r.mix.Add(t.sid, t.who, t.buf)
	ext := 0
	for _, h := range pub.Receiver().GetParameters().HeaderExtensions {
		if h.URI == sdp.AudioLevelURI {
			ext = h.ID
		}
	}
	slog.Info("voice: hearing", "identity", t.who, "track", t.sid, "name", pub.Name(), "audioLevelExt", ext != 0)
	go t.read(track, ext)
}

// read takes the track's packets, as they arrive, into its jitter buffer.
func (t *remote) read(track *webrtc.TrackRemote, ext int) {
	for {
		p, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if ext != 0 {
			if b := p.GetExtension(uint8(ext)); len(b) > 0 {
				t.levelExt.Store(int32(b[0] & 0x7f))
			}
		}
		now := time.Now()
		if t.arrival != nil {
			t.arrival(t.who, p.SequenceNumber, p.Timestamp, len(p.Payload), now)
		}
		t.buf.Push(jitter.Packet{Seq: p.SequenceNumber, TS: p.Timestamp, Payload: p.Payload}, now)
	}
}

func (r *room) unsubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		r.unsubscribedVideo(pub)
		return
	}
	r.mu.Lock()
	t := r.tracks[pub.SID()]
	delete(r.tracks, pub.SID())
	r.mu.Unlock()
	if t == nil {
		return
	}
	r.mix.Remove(t.sid)
	r.logTrack(t)
	t.buf.Close()
}

// PublishMic sends the voice read from mic as an Opus track.
func (r *room) PublishMic(mic rtc.Mic) error {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: opus.SampleRate, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1",
	})
	if err != nil {
		return err
	}
	enc, err := opus.NewEncoder(r.cfg.Opus)
	if err != nil {
		return err
	}
	pub, err := r.lk.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "microphone", Source: livekit.TrackSource_MICROPHONE})
	if err != nil {
		enc.Close()
		return fmt.Errorf("publish the mic: %w", err)
	}
	r.mu.Lock()
	r.pub = pub
	r.mu.Unlock()
	slog.Info("voice: publishing the mic", "track", pub.SID())
	go r.send(mic, track, enc)
	return nil
}

// send encodes the mic in 20 ms frames and writes them to the track. The
// RTP timestamp follows the mic's samples, sent or not, so a receiver sees
// silence (DTX, mute) as time going by. Of a run of DTX frames, the first
// is sent (it tells receivers the silence is on purpose) and then one every
// 400 ms.
func (r *room) send(mic rtc.Mic, track *lksdk.LocalSampleTrack, enc opus.Encoder) {
	defer enc.Close()
	frame := make([]float32, opus.FrameSamples)
	pkt := make([]byte, opus.MaxPacket)
	ts := rand.Uint32() | 1
	dtxRun, sentMuted := 0, false
	var lastTOC byte = 8 << 3 // until a packet is sent: SILK WB, 20 ms
	for {
		for h := 0; h < opus.FrameSamples; h += rtc.FrameSamples {
			if err := mic.Read(r.ctx, frame[h:h+rtc.FrameSamples]); err != nil {
				if r.ctx.Err() == nil {
					slog.Error("voice: the mic stopped", "err", err)
				}
				return
			}
		}
		level := pcm.LevelF32(frame)
		ts += opus.FrameSamples
		if r.muted.Load() {
			r.selfLevel.Store(int64(pcm.Silence * 100))
			if !sentMuted { // one DTX frame: receivers play silence, not concealment
				r.write(track, []byte{lastTOC}, ts, pcm.Silence)
				sentMuted = true
			}
			continue
		}
		sentMuted = false
		r.selfLevel.Store(int64(level * 100))
		n, err := enc.Encode(frame, pkt)
		if err != nil {
			slog.Error("voice: encode", "err", err)
			return
		}
		if opus.IsDTX(n) {
			dtxRun++
			if dtxRun%20 != 1 {
				continue
			}
		} else {
			dtxRun = 0
			lastTOC = pkt[0] &^ 3
		}
		r.write(track, pkt[:n], ts, level)
	}
}

// write sends one packet with its RFC 6464 level (0 is 0 dBov, 127 silence).
func (r *room) write(track *lksdk.LocalSampleTrack, p []byte, ts uint32, dbfs float64) {
	lvl := uint8(min(max(-dbfs, 0), 127))
	err := track.WriteSample(media.Sample{Data: p, Duration: 20 * time.Millisecond, PacketTimestamp: ts}, &lksdk.SampleWriteOptions{AudioLevel: &lvl})
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("voice: write", "err", err)
	}
}

// Mute stops sending the mic (and shows the track muted) without leaving.
func (r *room) Mute(muted bool) {
	if r.muted.Swap(muted) == muted {
		return
	}
	r.mu.Lock()
	pub := r.pub
	r.mu.Unlock()
	if pub != nil {
		pub.SetMuted(muted)
	}
	slog.Info("voice: mute", "muted", muted)
}

// Play mixes everyone else into spk until Leave.
func (r *room) Play(spk rtc.Speaker) error {
	if r.playing.Swap(true) {
		return errors.New("already playing")
	}
	r.mu.Lock()
	r.spk = spk
	r.mu.Unlock()
	go func() {
		if err := playout.Play(r.ctx, r.mix, spk); err != nil {
			slog.Error("voice: the speakers stopped", "err", err)
		}
	}()
	return nil
}

func (r *room) Participants() []rtc.Participant {
	var ps []rtc.Participant
	for _, p := range r.lk.GetRemoteParticipants() {
		ps = append(ps, participant(p))
	}
	return ps
}

// Stats sums each participant's tracks.
func (r *room) Stats() rtc.Stats {
	s := rtc.Stats{JitterDelay: map[string]time.Duration{}, Lost: map[string]int{}, Concealed: map[string]int{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tracks {
		st := t.buf.Stats()
		s.JitterDelay[t.who] = max(s.JitterDelay[t.who], st.Target)
		s.Lost[t.who] += st.Lost
		s.Concealed[t.who] += st.Concealed()
	}
	return s
}

// levels reports everyone's level ten times a second: remote tracks from
// the mix, the local one from the mic.
func (r *room) levels() {
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-r.quit:
			return
		case <-tk.C:
		}
		ls := []rtc.Level{}
		for _, l := range r.mix.Levels() {
			ls = append(ls, rtc.Level{Identity: l.Identity, DBFS: l.DBFS})
		}
		if r.pubOn() {
			ls = append(ls, rtc.Level{Identity: r.self.Identity, DBFS: float32(r.selfLevel.Load()) / 100})
		}
		if r.ev.Levels != nil {
			r.ev.Levels(ls)
		}
	}
}

func (r *room) pubOn() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pub != nil
}

// logStats writes each track's jitter buffer and stretch numbers to the
// log every so often.
func (r *room) logStats(every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-r.quit:
			return
		case <-tk.C:
		}
		r.mu.Lock()
		ts := make([]*remote, 0, len(r.tracks))
		for _, t := range r.tracks {
			ts = append(ts, t)
		}
		r.mu.Unlock()
		for _, t := range ts {
			r.logTrack(t)
		}
		for _, v := range r.videoList() {
			v.logStats()
		}
	}
}

func (r *room) videoList() []*videoTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	vs := make([]*videoTrack, 0, len(r.videos))
	for _, v := range r.videos {
		vs = append(vs, v)
	}
	return vs
}

func (r *room) logTrack(t *remote) {
	st := t.buf.Stats()
	ms := r.mix.Stats()[t.sid]
	r.mu.Lock()
	sfu := r.sfuLevel[t.who]
	r.mu.Unlock()
	slog.Info("voice: track", "identity", t.who, "jitterDelay", st.Target.Round(time.Millisecond), "level", st.Level.Round(time.Millisecond),
		"jitter", st.Jitter.Round(100*time.Microsecond), "received", st.Received, "lost", st.Lost, "fec", st.FEC, "plc", st.PLC,
		"late", st.Late, "underrunMs", st.Underrun*1000/opus.SampleRate, "skippedMs", st.Skipped*1000/opus.SampleRate,
		"stretchDrop", ms.Dropped, "stretchSilent", ms.Silent, "levelExt", t.levelExt.Load(), "sfuSpeakerLevel", sfu)
}

// TrackStats returns each remote track's jitter buffer numbers, by
// participant, for tools that report them (voice-check).
func TrackStats(rm rtc.Room) map[string]jitter.Stats {
	r, ok := rm.(*room)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]jitter.Stats{}
	for _, t := range r.tracks {
		out[t.who] = t.buf.Stats()
	}
	return out
}

// Leave hangs up.
func (r *room) Leave() {
	r.once.Do(func() {
		r.left.Store(true)
		r.cc.mu.Lock()
		pubs := append([]*videoPub(nil), r.cc.pubs...)
		r.cc.mu.Unlock()
		for _, p := range pubs {
			p.Stop()
		}
		r.cc.stop()
		close(r.quit)
		r.cancel()
		r.mu.Lock()
		ts := make([]*remote, 0, len(r.tracks))
		for _, t := range r.tracks {
			ts = append(ts, t)
		}
		r.mu.Unlock()
		for _, t := range ts {
			r.logTrack(t)
		}
		for _, v := range r.videoList() {
			v.logStats()
		}
		r.lk.Disconnect()
		slog.Info("voice: left the room")
	})
}
