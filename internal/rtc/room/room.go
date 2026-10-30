// Package room is the rtc interface over a media server. It publishes the
// mic as Opus with the RFC 6464 audio level on every packet (the SFU's
// speaker detection reads it), reads every remote audio track as raw RTP
// into its own jitter buffer, and mixes them for the speaker with the
// playout package; it sends and receives video too (video.go,
// videorecv.go). What it says to the server goes through a transport
// (transport.go): huddle's own SFU through sfuclient (sfu.go).
package room

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

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/jitter"
	"github.com/anthonyrego/huddle/internal/netsim"
	"github.com/anthonyrego/huddle/internal/opus"
	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/playout"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/icept"
	"github.com/anthonyrego/huddle/internal/video"
)

// Connector joins rooms through huddled's SFU at the join's URL. The
// zero value is the voice's defaults.
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
	// Downlink, when set, narrows what this side receives to that many bits
	// a second, with a 50-packet queue in front shared by everything a
	// connection receives (a test of the media server's allocation for one
	// viewer: huddle video -downlink); DownlinkFor, when set, lifts it that
	// long after joining.
	Downlink    int
	DownlinkFor time.Duration
	// SendOnly subscribes to nothing (bots that only publish).
	SendOnly bool
	// VideoFrame, when set, is told of every remote video frame as it is
	// shown, on the track's goroutine (a measuring hook: huddle video).
	// It must not keep the frame.
	VideoFrame func(t rtc.VideoTrack, f video.Frame, info VideoFrameInfo)
	// VideoPacket and VideoUnit, when set, are told of every remote video
	// packet as it's read (wire: when it came off the network, before a
	// Sim's delay; zero without one) and every access unit as it's put
	// together, on the track's reading goroutine (a debug trace: huddle
	// video -trace). VideoPacket must not keep the packet.
	VideoPacket func(t rtc.VideoTrack, p *rtp.Packet, wire, at time.Time)
	VideoUnit   func(t rtc.VideoTrack, u VideoUnitInfo)
}

// NewSim is a Connector.Sim with fixed settings and a fresh seed per stream.
func NewSim(loss float64, jitterAmount time.Duration, reorder float64) func() *netsim.Sim {
	return func() *netsim.Sim { return netsim.New(loss, jitterAmount, reorder, simSeed()) }
}

// simSeed gives each simulated stream its own random numbers.
func simSeed() uint64 { return uint64(time.Now().UnixNano()) }

// Connect joins the room the token is for.
func (c *Connector) Connect(ctx context.Context, j rtc.Join, ev rtc.Events) (rtc.Room, error) {
	r := &room{cfg: c, ev: ev, mix: playout.New(), tracks: map[string]*remote{}, videos: map[string]*videoTrack{}, sfuLevel: map[string]float32{}, quit: make(chan struct{})}
	r.cc = newVideoCC(r)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	// Our interceptors, nearest the network first: the receive side's
	// narrow downlink and simulated network, and the test uplink (the
	// transport puts congestion control after it).
	cfg := dialConfig{subscribe: !c.SendOnly}
	if c.Downlink > 0 {
		cfg.recv = append(cfg.recv, &icept.Downlink{BPS: c.Downlink, Slots: icept.UplinkSlots, Made: r.addDownlink})
		if c.DownlinkFor > 0 {
			time.AfterFunc(c.DownlinkFor, r.liftDownlink)
		}
	}
	if c.Sim != nil {
		cfg.recv = append(cfg.recv, icept.Sim{New: c.Sim})
	}
	if c.Uplink > 0 {
		cfg.send = append(cfg.send, &icept.Uplink{BPS: c.Uplink, Slots: icept.UplinkSlots, Made: r.cc.addUplink})
	}
	t, err := dialSFU(ctx, j, cfg, transportEvents{
		arrived: func(p rtc.Participant) {
			slog.Info("voice: arrived", "identity", p.Identity)
			if ev.Arrived != nil {
				ev.Arrived(p)
			}
		},
		left: func(identity string) {
			slog.Info("voice: left", "identity", identity)
			if ev.Left != nil {
				ev.Left(identity)
			}
		},
		subscribed:   r.subscribed,
		unsubscribed: r.unsubscribed,
		// The SFU's speaker detection runs on the RFC 6464 levels we send;
		// what it reports is kept for the log, as proof they arrive.
		speakers: func(ls map[string]float32) {
			r.mu.Lock()
			for who, l := range ls {
				r.sfuLevel[who] = max(r.sfuLevel[who], l)
			}
			r.mu.Unlock()
		},
		reconnecting: func() {
			slog.Warn("voice: reconnecting")
			if ev.Reconnecting != nil {
				ev.Reconnecting()
			}
		},
		reconnected: func() {
			slog.Info("voice: reconnected")
			if ev.Reconnected != nil {
				ev.Reconnected()
			}
		},
		disconnected: func(reason string) {
			if r.left.Load() {
				return
			}
			slog.Warn("voice: disconnected", "reason", reason)
			if ev.Disconnected != nil {
				ev.Disconnected(fmt.Errorf("disconnected: %v", reason))
			}
		},
	})
	if err != nil {
		r.cancel()
		return nil, fmt.Errorf("join the voice room: %w", err)
	}
	r.t = t
	r.self = t.self()
	others := r.Participants()
	slog.Info("voice: joined", "room", t.roomName(), "identity", r.self.Identity, "others", len(others))
	if ev.Joined != nil {
		ev.Joined(r.self, others)
	}
	go r.levels()
	if every := c.statsEvery(); every > 0 {
		go r.logStats(every)
	}
	return r, nil
}

// addDownlink keeps a connection's narrow downlink, to lift it later.
func (r *room) addDownlink(l *icept.RecvLink) {
	r.dlMu.Lock()
	r.downlinks = append(r.downlinks, l)
	r.dlMu.Unlock()
}

// liftDownlink takes the narrow downlink off every connection.
func (r *room) liftDownlink() {
	r.dlMu.Lock()
	dropped := 0
	for _, l := range r.downlinks {
		l.SetRate(0)
		dropped += l.Dropped()
	}
	r.dlMu.Unlock()
	slog.Info("video: downlink lifted", "after", r.cfg.DownlinkFor, "dropped", dropped)
}

// DownlinkDropped is how many packets the narrow downlink had no room for.
func DownlinkDropped(rm rtc.Room) int {
	r, ok := rm.(*room)
	if !ok {
		return 0
	}
	r.dlMu.Lock()
	defer r.dlMu.Unlock()
	n := 0
	for _, l := range r.downlinks {
		n += l.Dropped()
	}
	return n
}

func (c *Connector) statsEvery() time.Duration {
	if c.StatsEvery == 0 {
		return 10 * time.Second
	}
	return c.StatsEvery
}

type room struct {
	cfg    *Connector
	ev     rtc.Events
	t      transport // set once joined (its events don't need it)
	self   rtc.Participant
	mix    *playout.Mixer
	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}
	left   atomic.Bool
	once   sync.Once

	cc *videoCC

	dlMu      sync.Mutex
	downlinks []*icept.RecvLink // -downlink's, one a connection

	mu       sync.Mutex
	tracks   map[string]*remote     // by track SID
	videos   map[string]*videoTrack // by track SID
	spk      rtc.Speaker
	mic      audioOut
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

// subscribed starts playing a track we're sent, and returns what reads its
// RTCP.
func (r *room) subscribed(rt remoteTrack) func(rtcp.Packet) {
	if rt.kind == webrtc.RTPCodecTypeVideo {
		return r.subscribedVideo(rt)
	}
	if rt.kind != webrtc.RTPCodecTypeAudio {
		return nil
	}
	if !strings.EqualFold(rt.mime, webrtc.MimeTypeOpus) {
		slog.Warn("voice: not Opus, not played", "identity", rt.who, "codec", rt.mime)
		return nil
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		slog.Error("voice: decoder", "err", err)
		return nil
	}
	t := &remote{sid: rt.sid, who: rt.who, buf: jitter.New(dec, r.cfg.Jitter), arrival: r.cfg.Arrival}
	t.levelExt.Store(-1)
	r.mu.Lock()
	r.tracks[t.sid] = t
	r.mu.Unlock()
	r.mix.Add(t.sid, t.who, t.buf)
	slog.Info("voice: hearing", "identity", t.who, "track", t.sid, "name", rt.name, "audioLevelExt", rt.levelExt != 0)
	go t.read(rt.track, rt.levelExt)
	return t.sr.read
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
		t.sr.arrived(p.Timestamp, now, opus.SampleRate)
		t.buf.Push(jitter.Packet{Seq: p.SequenceNumber, TS: p.Timestamp, Payload: p.Payload}, now)
	}
}

// unsubscribed stops playing a track we're no longer sent.
func (r *room) unsubscribed(sid string) {
	r.unsubscribedVideo(sid)
	r.mu.Lock()
	t := r.tracks[sid]
	delete(r.tracks, sid)
	r.mu.Unlock()
	if t == nil {
		return
	}
	r.logTrack(t) // while the mix still has its numbers
	r.mix.Remove(t.sid)
	t.buf.Close()
}

// PublishMic sends the voice read from mic as an Opus track.
func (r *room) PublishMic(mic rtc.Mic) error {
	enc, err := opus.NewEncoder(r.cfg.Opus)
	if err != nil {
		return err
	}
	out, err := r.t.publishAudio()
	if err != nil {
		enc.Close()
		return fmt.Errorf("publish the mic: %w", err)
	}
	r.mu.Lock()
	r.mic = out
	r.mu.Unlock()
	slog.Info("voice: publishing the mic", "track", out.sid())
	go r.send(mic, out, enc)
	return nil
}

// send encodes the mic in 20 ms frames and writes them to the track. The
// RTP timestamp follows the mic's samples, sent or not, so a receiver sees
// silence (DTX, mute) as time going by. Of a run of DTX frames, the first
// is sent (it tells receivers the silence is on purpose) and then one every
// 400 ms.
func (r *room) send(mic rtc.Mic, out audioOut, enc opus.Encoder) {
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
				r.write(out, []byte{lastTOC}, ts, pcm.Silence)
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
		r.write(out, pkt[:n], ts, level)
	}
}

// write sends one packet with its RFC 6464 level (0 is 0 dBov, 127 silence).
func (r *room) write(out audioOut, p []byte, ts uint32, dbfs float64) {
	lvl := uint8(min(max(-dbfs, 0), 127))
	if err := out.write(p, ts, lvl); err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("voice: write", "err", err)
	}
}

// Mute stops sending the mic (and shows the track muted) without leaving.
func (r *room) Mute(muted bool) {
	if r.muted.Swap(muted) == muted {
		return
	}
	r.mu.Lock()
	out := r.mic
	r.mu.Unlock()
	if out != nil {
		out.setMuted(muted)
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

func (r *room) Participants() []rtc.Participant { return r.t.participants() }

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
	return r.mic != nil
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
		r.t.leave()
		slog.Info("voice: left the room")
	})
}
