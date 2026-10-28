// Package agent is the whiteboard: one per active room, run by huddled
// serve. It joins the room's media (media.go: LiveKit, or huddled's own
// SFU) as an agent participant and listens to each person's voice
// (ears.go: Opus decoded in pure Go, a voice detector, and a streaming
// transcription session per person). The transcript goes through the gate
// (internal/gate); what the gate lets through, and typed commands from the
// apps, queue for the brain (one gemini Live session per room, brain.go),
// one command in flight at a time. What the brain draws becomes board
// versions in the store (versions.go), laid out here and sent to every
// app. Its voice is published on its own track only for answers the voice
// policy allows (voice.go).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/live"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Models the whiteboard uses.
const (
	BrainModel   = live.DefaultModel
	EarModel     = "gemini-3.5-transcribe-live"
	SummaryModel = "gemini-3.5-flash-lite"
)

// Identity is the whiteboard's participant identity in every room.
const Identity = "whiteboard"

// Config is what an agent runs with.
type Config struct {
	Room   string
	Store  Store
	Hub    Hub
	APIKey string
	// Media is how the whiteboard joins the room's media.
	Media Media

	BrainModel, EarModel, SummaryModel string // the defaults when empty
	// Voice is the policy when the meeting has none set (VoiceAnswers
	// when empty).
	Voice string
	// AutoEvery is how much new speech brings an auto-mode update turn.
	AutoEvery time.Duration
	// Idle is how long the agent stays in a room nobody is in.
	Idle time.Duration
	// ReconnectEvery, when set, replaces the brain's connection this
	// often as if the server had said GoAway (to exercise resumption).
	ReconnectEvery time.Duration
	// NewEncoder makes the Opus encoder for the voice (NewEncoder when nil).
	NewEncoder func() (Encoder, error)
	// Trace, when set, hears everything the agent does (agent-check).
	Trace func(Event)
	Log   *slog.Logger
}

// Event is something the agent did, for Trace.
type Event struct {
	Kind    string // see the Ev* constants
	AtMs    int64  // meeting time
	Command string // the command id, for turn events
	Speaker string
	Text    string
	Seq     int64 // version, for EvVersion
	N       int   // a count: ops, audio frames, tokens
	Detail  string
}

// Trace event kinds.
const (
	EvSegment   = "segment"    // a final transcript segment (Speaker, Text)
	EvListening = "listening"  // the gate opened for Speaker
	EvCommand   = "command"    // the gate let a command through (Command, Speaker, Text, Detail: source; N: start ms)
	EvStop      = "stop"       // a stop (Speaker)
	EvTurn      = "turn"       // a turn started (Command; Detail "auto" for auto turns)
	EvVersion   = "version"    // a version committed (Command, Seq, N ops)
	EvSpeak     = "speak"      // the turn called speak (Command)
	EvAudio     = "audio"      // a turn's voice was published (Command, N 20 ms frames)
	EvTurnEnd   = "turn-end"   // a turn ended (Command, N ops, Detail: why)
	EvAuto      = "auto"       // auto mode changed (Detail on/off)
	EvConnect   = "connect"    // the brain connected (Detail: fresh or resumed)
	EvGoAway    = "go-away"    // the brain's connection is ending
	EvRotate    = "rotate"     // a transcription session was replaced (Speaker)
	EvUsage     = "usage"      // tokens (N), Detail: which model and kind
	EvError     = "error"      // Detail
	EvVoiceDrop = "voice-drop" // audio the policy kept out (Command, N bytes)
)

// Agent is one room's whiteboard.
type Agent struct {
	cfg     Config
	log     *slog.Logger
	meeting store.Meeting
	policy  string

	in   chan func() // work for the loop, from other goroutines
	gate *gate.Gate
	b    boardState

	queue    []queued
	turn     *turn
	auto     bool
	autoNew  time.Duration // speech heard since the last auto turn
	lastTurn int64         // meeting ms of the last turn's start
	segs     []store.Segment
	summary  string
	summedTo int64 // the summary covers the transcript up to here
	summing  bool
	runID    string

	brain brain
	voice *voiceOut
	room  MediaRoom

	status   wire.AgentStatus
	names    map[string]string // identity -> display name
	namesMu  sync.Mutex
	lastSeen time.Time // someone was in the room
}

type queued struct {
	cmd     gate.Command
	auto    bool
	retried bool // sent once already, on a connection that closed
	at      time.Time
}

// holdFor is the longest a finished command waits for one begun before
// it to finish, so two said at once are served in the order they were
// begun.
const holdFor = 4 * time.Second

// New makes an agent; Run runs it.
func New(cfg Config) *Agent {
	if cfg.BrainModel == "" {
		cfg.BrainModel = BrainModel
	}
	if cfg.EarModel == "" {
		cfg.EarModel = EarModel
	}
	if cfg.SummaryModel == "" {
		cfg.SummaryModel = SummaryModel
	}
	if cfg.AutoEvery == 0 {
		cfg.AutoEvery = 40 * time.Second
	}
	if cfg.Idle == 0 {
		cfg.Idle = 2 * time.Minute
	}
	if cfg.NewEncoder == nil {
		cfg.NewEncoder = NewEncoder
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Agent{
		cfg:   cfg,
		log:   log.With("room", cfg.Room),
		in:    make(chan func(), 1024),
		gate:  gate.New(gate.Config{}),
		b:     boardState{room: cfg.Room, st: cfg.Store, hub: cfg.Hub},
		names: map[string]string{},
		runID: fmt.Sprintf("%x", time.Now().Unix()%0xfffff),
	}
}

// ms is a wall time on the meeting's clock.
func (a *Agent) ms(t time.Time) int64 { return t.Sub(a.meeting.StartedAt).Milliseconds() }

func (a *Agent) now() int64 { return a.ms(time.Now()) }

func (a *Agent) trace(e Event) {
	if a.cfg.Trace != nil {
		if e.AtMs == 0 {
			e.AtMs = a.now()
		}
		a.cfg.Trace(e)
	}
}

// post runs f on the loop.
func (a *Agent) post(f func()) {
	select {
	case a.in <- f:
	default:
		a.log.Warn("agent: loop is behind; dropping work")
	}
}

// Run joins the room and runs until ctx ends or the room has been empty
// for Config.Idle.
func (a *Agent) Run(ctx context.Context) error {
	if a.cfg.APIKey == "" {
		return errors.New("agent: no GEMINI_API_KEY")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var err error
	if a.meeting, err = a.cfg.Store.EnsureMeeting(ctx, a.cfg.Room); err != nil {
		return err
	}
	a.policy, _ = a.cfg.Store.Voice(ctx, a.cfg.Room)
	if a.policy == "" {
		a.policy = a.cfg.Voice
	}
	if a.policy == "" {
		a.policy = VoiceAnswers
	}
	if err := a.b.load(ctx); err != nil {
		return err
	}
	if st, err := a.cfg.Store.AgentState(ctx, a.cfg.Room); err == nil {
		a.brain.handle, a.summary = st.ResumeHandle, st.Summary
	}
	a.lastTurn = a.now()
	a.summedTo = a.lastTurn
	a.lastSeen = time.Now()

	enc, err := a.cfg.NewEncoder()
	if err != nil {
		return fmt.Errorf("agent: voice encoder: %w", err)
	}
	a.voice = newVoiceOut(enc, func(on bool) { a.post(func() { a.onSpeaking(on) }) })
	defer a.voice.close()
	if a.room, err = a.join(ctx); err != nil {
		return err
	}
	defer a.room.Leave()
	go a.voice.run(ctx, a.room.Publish)

	a.brain.connect(ctx, a)
	a.log.Info("agent: in the room", "head", a.b.head, "voice", a.policy, "resume", a.brain.handle != "")
	a.setStatus()
	defer a.cfg.Hub.Send(a.cfg.Room, wire.KindAgentStatus, wire.AgentStatus{Room: a.cfg.Room, State: wire.AgentOffline})

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.brain.close()
			return nil
		case f := <-a.in:
			f()
		case r := <-a.brain.ready:
			a.brain.connected(ctx, a, r)
		case ev, ok := <-a.brain.events():
			a.onBrain(ctx, ev, ok)
		case <-tick.C:
			a.tick(ctx)
			if a.room.People() > 0 || a.cfg.Hub.Count(a.cfg.Room) > 0 {
				a.lastSeen = time.Now()
			} else if time.Since(a.lastSeen) > a.cfg.Idle && a.turn == nil {
				a.log.Info("agent: the room is empty; leaving")
				a.brain.close()
				return nil
			}
		}
	}
}

// name is a participant's display name.
func (a *Agent) name(identity string) string {
	a.namesMu.Lock()
	n := a.names[identity]
	a.namesMu.Unlock()
	if n != "" {
		return n
	}
	// A dev identity is the name's slug and a suffix: "sam-1a2b" is Sam.
	base := identity
	if i := strings.LastIndexByte(base, '-'); i > 0 {
		base = base[:i]
	}
	base = strings.ReplaceAll(base, "-", " ")
	if base == "" {
		return identity
	}
	r := []rune(base)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func (a *Agent) setName(identity, name string) {
	if name == "" {
		return
	}
	a.namesMu.Lock()
	a.names[identity] = name
	a.namesMu.Unlock()
}

// Deliver takes a frame an app sent: a typed command or the
// push-to-address key.
func (a *Agent) Deliver(identity string, e wire.Envelope) {
	switch e.Kind {
	case wire.KindCommand:
		var c wire.Command
		if e.Decode(&c) != nil {
			return
		}
		a.post(func() { a.gateOut(a.gate.Typed(identity, a.name(identity), c.Text, a.now())) })
	case wire.KindAddress:
		var ad wire.Address
		if e.Decode(&ad) != nil {
			return
		}
		a.post(func() { a.gateOut(a.gate.Address(identity, a.name(identity), ad.Held, a.now())) })
	}
}

// onSegment takes a piece of the transcript from an ear.
func (a *Agent) onSegment(g gate.Segment) {
	if g.Final && strings.TrimSpace(g.Text) != "" {
		seg := store.Segment{Meeting: a.cfg.Room, Identity: g.Identity, Speaker: g.Speaker, StartMs: g.StartMs, EndMs: g.EndMs, Text: g.Text}
		a.segs = append(a.segs, seg)
		if len(a.segs) > 400 {
			a.segs = a.segs[len(a.segs)-400:]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.cfg.Store.AddSegment(ctx, seg); err != nil {
			a.log.Warn("agent: store segment", "err", err)
		}
		cancel()
		if a.auto {
			a.autoNew += time.Duration(g.EndMs-g.StartMs) * time.Millisecond
		}
		a.trace(Event{Kind: EvSegment, Speaker: g.Speaker, Text: g.Text, AtMs: g.EndMs, N: int(g.StartMs)})
	}
	if g.Final && strings.TrimSpace(g.Text) == "" {
		// The end of speech with nothing transcribed: only the gate cares.
	} else {
		a.cfg.Hub.Send(a.cfg.Room, wire.KindTranscript, wire.Segment{
			Room: a.cfg.Room, Speaker: g.Speaker, Identity: g.Identity, StartMs: g.StartMs, EndMs: g.EndMs, Text: g.Text, Final: g.Final,
		})
	}
	a.gateOut(a.gate.Segment(g))
	a.maybeSummarize()
}

// gateOut acts on what the gate decided.
func (a *Agent) gateOut(outs []gate.Out) {
	for _, o := range outs {
		switch o.Kind {
		case gate.Listening:
			a.trace(Event{Kind: EvListening, Speaker: o.Speaker})
		case gate.Done:
			c := o.Command
			c.ID = a.runID + "-" + c.ID
			a.trace(Event{Kind: EvCommand, Command: c.ID, Speaker: c.Speaker, Text: c.Text, Detail: string(c.Source), N: int(c.StartMs), AtMs: c.AtMs})
			if c.Stop {
				a.stop(c)
				continue
			}
			a.log.Info("agent: command", "id", c.ID, "speaker", c.Speaker, "source", c.Source, "text", c.Text)
			a.enqueue(queued{cmd: c})
		}
	}
	a.setStatus()
}

// stop cuts off a spoken answer and ends auto mode.
func (a *Agent) stop(c gate.Command) {
	a.log.Info("agent: stop", "speaker", c.Speaker)
	a.trace(Event{Kind: EvStop, Speaker: c.Speaker, Command: c.ID})
	a.voice.cut()
	if a.turn != nil {
		a.turn.muted = true
	}
	if a.auto {
		a.setAuto(false)
	}
	// Auto turns that haven't started are dropped with it.
	a.queue = slices.DeleteFunc(a.queue, func(q queued) bool { return q.auto })
}

// enqueue adds a command; waiting commands go in the order they were
// said, auto-mode turns behind every real one.
func (a *Agent) enqueue(q queued) {
	q.at = time.Now()
	a.queue = append(a.queue, q)
	slices.SortStableFunc(a.queue, func(x, y queued) int {
		if x.auto != y.auto {
			if x.auto {
				return 1
			}
			return -1
		}
		return int(x.cmd.StartMs - y.cmd.StartMs)
	})
}

func (a *Agent) setAuto(on bool) {
	if a.auto == on {
		return
	}
	a.auto = on
	a.autoNew = 0
	a.log.Info("agent: auto mode", "on", on)
	detail := "off"
	if on {
		detail = "on"
	}
	a.trace(Event{Kind: EvAuto, Detail: detail})
	a.setStatus()
}

func (a *Agent) onSpeaking(on bool) {
	a.gate.SetSpeaking(on)
	a.setStatus()
}

// tick moves things along: the gate's pauses, the next command, auto
// mode, a brain that needs reconnecting.
func (a *Agent) tick(ctx context.Context) {
	a.gateOut(a.gate.Tick(a.now()))
	if a.turn != nil {
		a.turn.check(ctx, a)
		return
	}
	a.brain.maybeReconnect(ctx, a)
	if a.auto && len(a.queue) == 0 && a.autoNew >= a.cfg.AutoEvery {
		a.autoNew = 0
		a.enqueue(queued{auto: true, cmd: gate.Command{ID: fmt.Sprintf("%s-auto%d", a.runID, a.now()/1000), Speaker: "auto", StartMs: a.now(), EndMs: a.now()}})
	}
	if len(a.queue) > 0 && a.brain.ready_() {
		q := a.queue[0]
		if start, ok := a.gate.OldestOpen(); ok && !q.auto && start < q.cmd.StartMs && time.Since(q.at) < holdFor {
			return // someone began a command before this one and is still saying it
		}
		a.queue = a.queue[1:]
		a.startTurn(ctx, q)
	}
	a.setStatus()
}

// setStatus tells the apps what the whiteboard is doing, when it changes.
func (a *Agent) setStatus() {
	s := wire.AgentStatus{Room: a.cfg.Room, State: wire.AgentIdle, Queued: len(a.queue)}
	listening := ""
	for _, id := range a.listeners() {
		listening = a.name(id)
		break
	}
	switch {
	case a.voice != nil && a.voice.speaking():
		s.State = wire.AgentSpeaking
		if a.turn != nil {
			s.Detail = "for " + a.turn.cmd.Speaker
		}
	case a.turn != nil && !a.turn.auto:
		s.State = wire.AgentWorking
		s.Detail = "for " + a.turn.cmd.Speaker + ": " + shorten(a.turn.cmd.Text, 40)
	case listening != "":
		s.State, s.Detail = wire.AgentListening, listening
	case a.auto:
		s.State = wire.AgentAuto
	case !a.brain.ready_() && !a.brain.ever:
		s.State, s.Detail = wire.AgentIdle, "connecting"
	}
	if s != a.status {
		a.status = s
		a.cfg.Hub.Send(a.cfg.Room, wire.KindAgentStatus, s)
	}
}

// listeners are the identities the gate is open for, sorted.
func (a *Agent) listeners() []string {
	return a.gate.Open()
}

func shorten(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
