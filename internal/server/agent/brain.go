package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/live"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/wire"
)

// The brain's context: compressed to compressTo tokens when it reaches
// compressAt, so a meeting can run as long as it likes.
const (
	compressAt = 32000
	compressTo = 16000
)

// brain is the room's realtime session and its reconnection. A Live
// connection lasts about ten minutes: the server says GoAway first, and
// the agent reconnects with the newest resumption handle at the next
// moment no turn is in flight.
type brain struct {
	sess       *live.Session
	handle     string
	ready      chan connectResult
	connecting bool
	goAway     bool
	ever       bool // connected at least once
	since      time.Time
	failures   int
	retryAt    time.Time
}

type connectResult struct {
	sess    *live.Session
	resumed bool
	err     error
}

func (b *brain) events() <-chan live.Event {
	if b.sess == nil {
		return nil
	}
	return b.sess.Events()
}

func (b *brain) ready_() bool { return b.sess != nil && !b.connecting }

// connect opens a session in the background, resuming if there's a
// handle; the loop gets it on ready.
func (b *brain) connect(ctx context.Context, a *Agent) {
	if b.connecting {
		return
	}
	if b.ready == nil {
		b.ready = make(chan connectResult, 1)
	}
	b.connecting = true
	handle := b.handle
	if b.sess != nil {
		// A session can't be resumed while its old connection is open:
		// close it first (this only happens between turns).
		b.sess.Close()
		b.sess = nil
	}
	log := a.log
	cfg := live.Config{
		Model:      a.cfg.BrainModel,
		System:     system(a.policy),
		Tools:      board.Tools(),
		CompressAt: compressAt,
		CompressTo: compressTo,
	}
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cfg.Resume = handle
		s, err := live.Connect(cctx, a.cfg.APIKey, cfg)
		resumed := handle != ""
		if err != nil && handle != "" {
			// A handle that no longer works: start fresh (every turn
			// carries the summary and the board).
			log.Warn("agent: resuming the brain failed; starting fresh", "err", err)
			cfg.Resume = ""
			s, err = live.Connect(cctx, a.cfg.APIKey, cfg)
			resumed = false
		}
		b.ready <- connectResult{s, resumed, err}
	}()
}

// connected takes a new session in place of the old one.
func (b *brain) connected(ctx context.Context, a *Agent, r connectResult) {
	b.connecting = false
	if r.err != nil {
		b.failures++
		b.retryAt = time.Now().Add(time.Duration(min(b.failures, 5)) * 2 * time.Second)
		a.log.Error("agent: brain connect", "err", r.err, "retry", b.retryAt.Sub(time.Now()).Round(time.Second))
		a.trace(Event{Kind: EvError, Detail: "brain connect: " + r.err.Error()})
		return
	}
	b.sess, b.goAway, b.failures, b.ever, b.since = r.sess, false, 0, true, time.Now()
	how := "fresh"
	if r.resumed {
		how = "resumed"
	} else {
		b.handle = ""
	}
	a.log.Info("agent: brain connected", "how", how)
	a.trace(Event{Kind: EvConnect, Detail: how})
	a.setStatus()
}

// maybeReconnect reconnects between turns when the server is about to
// close the connection, or has.
func (b *brain) maybeReconnect(ctx context.Context, a *Agent) {
	if b.connecting || time.Now().Before(b.retryAt) {
		return
	}
	if e := a.cfg.ReconnectEvery; e > 0 && b.sess != nil && !b.goAway && time.Since(b.since) > e {
		a.log.Info("agent: reconnecting the brain on schedule", "every", e)
		b.goAway = true
	}
	if b.sess == nil || b.goAway {
		b.connect(ctx, a)
	}
}

func (b *brain) close() {
	if b.sess != nil {
		b.sess.Close()
		b.sess = nil
	}
}

// turn is one command with the brain.
type turn struct {
	cmd      gate.Command
	auto     bool
	text     string
	started  time.Time
	lastEv   time.Time
	doneAt   time.Time // the last turn-complete; zero while generating
	spoke    bool      // it called speak
	muted    bool      // someone said stop
	awaiting bool      // a non-blocking answer went WHEN_IDLE: another generation follows
	ops      int
	versions int
	frames   int
	said     strings.Builder
	retried  bool
}

// startTurn sends a command to the brain.
func (a *Agent) startTurn(ctx context.Context, q queued) {
	now := a.now()
	var recent []store.Segment
	for _, s := range a.segs {
		if s.EndMs > a.lastTurn && !a.queuedWords(s) {
			recent = append(recent, s)
		}
	}
	recent = recentText(recent, 6000)
	t := &turn{cmd: q.cmd, auto: q.auto, started: time.Now(), lastEv: time.Now(), retried: q.retried}
	t.text = turnText(q.cmd, a.b.head, recent, a.summary, a.b.doc, q.auto, now)
	a.lastTurn = now
	a.turn = t
	detail := ""
	if q.auto {
		detail = "auto"
	}
	a.trace(Event{Kind: EvTurn, Command: q.cmd.ID, Speaker: q.cmd.Speaker, Text: q.cmd.Text, Detail: detail})
	a.log.Info("agent: turn", "id", q.cmd.ID, "auto", q.auto, "recent", len(recent))
	a.brain.sess.SendTurn(t.text, true)
	a.setStatus()
}

// queuedWords reports whether a segment is a command still waiting its
// turn: it comes as its own turn, so it stays out of the others'.
func (a *Agent) queuedWords(s store.Segment) bool {
	for _, q := range a.queue {
		c := q.cmd
		if !q.auto && c.Identity == s.Identity && s.StartMs < c.EndMs && s.EndMs > c.StartMs {
			return true
		}
	}
	return false
}

// check ends a turn the model has gone quiet on.
func (t *turn) check(ctx context.Context, a *Agent) {
	switch {
	case !t.doneAt.IsZero() && time.Since(t.lastEv) > 4*time.Second:
		a.endTurn("quiet after turn-complete")
	case time.Since(t.started) > 60*time.Second:
		a.endTurn("timeout")
	case time.Since(t.lastEv) > 20*time.Second:
		a.endTurn("no reply")
	}
}

func (a *Agent) endTurn(why string) {
	t := a.turn
	if t == nil {
		return
	}
	a.turn = nil
	if t.frames > 0 {
		a.trace(Event{Kind: EvAudio, Command: t.cmd.ID, N: t.frames})
	}
	a.trace(Event{Kind: EvTurnEnd, Command: t.cmd.ID, N: t.ops, Detail: why})
	a.log.Info("agent: turn done", "id", t.cmd.ID, "ops", t.ops, "versions", t.versions, "spoke", t.spoke, "frames", t.frames, "why", why,
		"took", time.Since(t.started).Round(time.Millisecond))
	if said := strings.TrimSpace(t.said.String()); said != "" && t.frames > 0 {
		// What it said aloud is part of the meeting's transcript.
		now := a.now()
		a.onSegment(gate.Segment{Identity: Identity, Speaker: "Whiteboard", Text: said, StartMs: now - int64(t.frames)*20, EndMs: now, Final: true})
	}
	a.setStatus()
}

// onBrain takes what the brain said.
func (a *Agent) onBrain(ctx context.Context, ev live.Event, ok bool) {
	b := &a.brain
	if !ok || ev.Kind == live.Closed {
		if ok && ev.Err != nil {
			a.log.Warn("agent: brain closed", "err", ev.Err)
			a.trace(Event{Kind: EvError, Detail: "brain closed: " + ev.Err.Error()})
		}
		b.sess = nil
		if t := a.turn; t != nil {
			// Send it again on the next connection, unless it already drew.
			a.turn = nil
			if t.versions == 0 && !t.retried {
				a.queue = append([]queued{{cmd: t.cmd, auto: t.auto, retried: true}}, a.queue...)
				a.trace(Event{Kind: EvTurnEnd, Command: t.cmd.ID, Detail: "retry: the connection closed"})
			} else {
				a.trace(Event{Kind: EvTurnEnd, Command: t.cmd.ID, N: t.ops, Detail: "the connection closed"})
			}
		}
		b.connect(ctx, a)
		return
	}
	t := a.turn
	if t != nil {
		t.lastEv = time.Now()
	}
	switch ev.Kind {
	case live.ToolCall:
		if t == nil {
			// Calls with no turn (a late answer): answer them, draw nothing.
			var rs []*genai.FunctionResponse
			for _, c := range ev.Calls {
				rs = append(rs, &genai.FunctionResponse{ID: c.ID, Name: c.Name, Scheduling: genai.FunctionResponseSchedulingSilent,
					Response: map[string]any{"error": "that command is over"}})
			}
			b.sess.SendToolResponse(rs)
			return
		}
		t.doneAt = time.Time{}
		a.calls(ctx, t, ev.Calls)
	case live.Audio:
		if t == nil {
			return
		}
		t.doneAt = time.Time{}
		if a.voiceAllowed(t) {
			a.voice.push(ev.Data, ev.Rate)
			t.frames += len(ev.Data) / 2 * 50 / ev.Rate
		} else {
			a.trace(Event{Kind: EvVoiceDrop, Command: t.cmd.ID, N: len(ev.Data)})
		}
	case live.OutputText:
		if t != nil && a.voiceAllowed(t) {
			t.said.WriteString(ev.Text)
		}
	case live.TurnComplete:
		if t == nil {
			return
		}
		t.doneAt = time.Now()
		if t.awaiting {
			// Another generation follows (or, if none comes, check ends it).
			t.awaiting = false
			return
		}
		a.endTurn("turn-complete")
	case live.GoAway:
		b.goAway = true
		a.log.Info("agent: brain go-away", "left", ev.TimeLeft)
		a.trace(Event{Kind: EvGoAway, Detail: ev.TimeLeft.String()})
	case live.Resumable:
		b.handle = ev.Handle
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := a.cfg.Store.SetAgentState(sctx, a.cfg.Room, store.AgentState{ResumeHandle: ev.Handle, Summary: a.summary}); err != nil {
			a.log.Warn("agent: store resumption handle", "err", err)
		}
		cancel()
	case live.Usage:
		if u := ev.Usage; u != nil {
			a.trace(Event{Kind: EvUsage, N: int(u.PromptTokenCount), Detail: "brain prompt"})
			a.trace(Event{Kind: EvUsage, N: int(u.ResponseTokenCount), Detail: "brain response"})
		}
	}
}

// voiceAllowed is whether the turn's voice may be heard.
func (a *Agent) voiceAllowed(t *turn) bool {
	if t.muted || t.auto {
		return false
	}
	switch a.policy {
	case VoiceMuted:
		return false
	case VoiceNarrate:
		return true
	}
	return t.spoke
}

// calls answers one message of tool calls: the drawing becomes one new
// version at once, so the room sees it as soon as the model sends it.
func (a *Agent) calls(ctx context.Context, t *turn, calls []*genai.FunctionCall) {
	doc := clone(a.b.doc)
	var changed, points []board.Op
	type answer struct {
		r        *genai.FunctionResponse
		failed   bool
		blocking bool
	}
	answers := make([]answer, 0, len(calls))
	commit := func() {
		if len(changed) == 0 {
			return
		}
		ops := changed
		c := change{doc: doc, ops: ops, requester: t.cmd.Speaker, command: t.cmd.ID, tMs: a.now(), restored: -1,
			redo: func(head *board.Doc) (*board.Doc, []board.Op) { return applyOps(head, ops) }}
		v, err := a.b.commit(ctx, c)
		if err != nil {
			a.log.Error("agent: commit", "err", err)
			a.trace(Event{Kind: EvError, Detail: "commit: " + err.Error()})
		} else {
			t.versions++
			t.ops += len(ops)
			a.trace(Event{Kind: EvVersion, Command: t.cmd.ID, Seq: v.Seq, N: len(ops)})
		}
		changed = nil
		doc = clone(a.b.doc)
	}
	for _, c := range calls {
		r := &genai.FunctionResponse{ID: c.ID, Name: c.Name}
		ans := answer{r: r}
		op, err := board.Decode(c.Name, c.Args)
		switch {
		case err != nil:
		case op.Name == board.Speak:
			t.spoke = true
			a.trace(Event{Kind: EvSpeak, Command: t.cmd.ID})
			r.Response = map[string]any{"ok": true}
		case op.Name == board.GetBoard:
			ans.blocking = true
			r.Response = map[string]any{"board": fmt.Sprintf("version %d\n%s", a.b.head, doc.Digest())}
		case op.Name == board.Transcript:
			ans.blocking = true
			r.Response = map[string]any{"transcript": a.transcript(ctx, op.FromMs, op.ToMs)}
		case op.Name == board.Auto:
			if t.auto && !op.Enabled {
				err = fmt.Errorf("auto mode stays on until someone says stop")
				break
			}
			a.setAuto(op.Enabled)
			r.Response = map[string]any{"ok": true, "auto": op.Enabled}
		case op.Versioned():
			commit()
			var target int64
			if op.Name == board.Undo {
				target, err = a.b.undoTarget(ctx)
				if err == nil && target < 0 {
					err = fmt.Errorf("there's nothing to undo")
				}
			} else {
				target = op.Version
			}
			if err != nil {
				break
			}
			var v store.Version
			v, err = a.b.restore(ctx, op, target, t.cmd.Speaker, t.cmd.ID, a.now())
			if err == nil {
				t.versions++
				t.ops++
				a.trace(Event{Kind: EvVersion, Command: t.cmd.ID, Seq: v.Seq, N: 1})
				r.Response = map[string]any{"ok": true, "version": v.Seq, "same_as": target}
				doc = clone(a.b.doc)
			}
		case op.Name == board.PointAt:
			if err = doc.Apply(op); err == nil {
				points = append(points, op)
				r.Response = map[string]any{"ok": true}
			}
		default:
			if err = doc.Apply(op); err == nil {
				changed = append(changed, op)
				r.Response = map[string]any{"ok": true}
			}
		}
		if err != nil {
			ans.failed = true
			r.Response = map[string]any{"error": err.Error()}
			a.log.Info("agent: call refused", "call", c.Name, "err", err)
		}
		answers = append(answers, ans)
	}
	commit()
	if len(points) > 0 {
		// point_at changes nothing: its ops go out for the version shown.
		raw := make([]json.RawMessage, len(points))
		for i, p := range points {
			raw[i], _ = json.Marshal(p)
		}
		a.cfg.Hub.Send(a.cfg.Room, wire.KindBoardOps, wire.BoardOps{Room: a.cfg.Room, Seq: a.b.head, Ops: raw})
	}

	// Drawing that went through needs nothing more from the model, so it
	// is answered SILENT (answered WHEN_IDLE, the model starts another
	// generation per answer: AGENTS.md). A refusal, a read, speak and
	// narration are answered WHEN_IDLE so the model can fix, use, say.
	var rs []*genai.FunctionResponse
	t.awaiting = false
	for _, ans := range answers {
		idle := ans.failed || ans.blocking || t.spoke || a.policy == VoiceNarrate && !t.auto
		ans.r.Scheduling = genai.FunctionResponseSchedulingSilent
		if idle {
			ans.r.Scheduling = genai.FunctionResponseSchedulingWhenIdle
			if !ans.blocking {
				t.awaiting = true
			}
		}
		rs = append(rs, ans.r)
	}
	a.brain.sess.SendToolResponse(rs)
	a.setStatus()
}

// transcript is the stored transcript between two meeting times, as text.
func (a *Agent) transcript(ctx context.Context, from, to int64) string {
	if to <= from {
		to = a.now()
	}
	segs, err := a.cfg.Store.Segments(ctx, a.cfg.Room, from, to)
	if err != nil {
		return "couldn't read the transcript: " + err.Error()
	}
	if len(segs) == 0 {
		return "nothing was said then"
	}
	segs = recentText(segs, 12000)
	var b strings.Builder
	for _, s := range segs {
		fmt.Fprintf(&b, "[%s] %s: %s\n", clockText(s.StartMs), s.Speaker, s.Text)
	}
	return b.String()
}
