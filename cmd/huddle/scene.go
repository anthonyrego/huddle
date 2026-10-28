package main

// The meeting window: the board stage filling it, a status pill at the top
// left (the connection and the whiteboard), and the bar along the bottom
// (bar.go: the room, and the voice session from Track V).

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/theme"
	"github.com/anthonyrego/huddle/internal/ui"
	"github.com/anthonyrego/huddle/internal/ui/boardview"
	"github.com/anthonyrego/huddle/internal/wire"
)

// meetingScene is the window's content. All methods run on the main thread.
type meetingScene struct {
	shaper *text.Shaper
	scale  func() float32
	mt     *meeting
	view   *boardview.View
	host   ui.Host
	tc     textCache
	st     meetingState // the copy last taken
	nops   int          // ops on the board shown
	more   bool         // a widget asked for another frame

	// translate turns SDL events into ui events (the window's); send
	// queues a frame for the server (nil without a connection).
	translate func(*sdl.Event) (ui.Event, bool)
	send      func(kind string, body any) bool

	// call is the voice session (nil without a room); muteRect is where the
	// bar drew the mic, for clicks (bar.go).
	call     *call
	muteRect draw.Rect
}

func newMeetingScene(shaper *text.Shaper, scale func() float32, mt *meeting) *meetingScene {
	s := &meetingScene{shaper: shaper, scale: scale, mt: mt, view: boardview.New()}
	s.host.W = s.view
	s.view.OnClick = func(id string) { slog.Info("board: clicked", "id", id) }
	return s
}

func (s *meetingScene) ctx(pal theme.Palette, now time.Time) *ui.Ctx {
	return &ui.Ctx{Text: s.shaper, Pal: pal, Scale: s.scale(), Now: now, Animate: func() { s.more = true }}
}

func (s *meetingScene) draw(l *draw.List, w, h float32, pal theme.Palette, now time.Time) {
	s.more = false
	c := s.ctx(pal, now)
	stage := draw.Rect{W: w, H: h - barH}
	s.host.Draw(c, l, stage)
	s.drawStatus(c, l)
	s.drawBar(c, l, draw.Rect{Y: h - barH, W: w, H: barH})
}

// drawStatus is the pill at the top left: the connection's state, and what
// the whiteboard is doing once it's in the room.
func (s *meetingScene) drawStatus(c *ui.Ctx, l *draw.List) {
	status := s.st.conn
	if status == "" {
		status = connOffline
	}
	dot := c.Pal.TextFaint
	switch s.st.conn {
	case connConnected, connFixture:
		dot = c.Pal.Success
	case connRetrying, connOffline:
		dot = c.Pal.Error
	}
	if a := agentLine(s.st.agent); a != "" && s.st.conn == connConnected {
		status = a
		if s.st.agent.State == wire.AgentListening || s.st.agent.State == wire.AgentSpeaking {
			dot = c.Pal.Accent
		}
	}
	lay := s.tc.lay(c, status, text.SansSemibold, 12.5)
	r := draw.Rect{X: 16, Y: 14, W: lay.Width + 34, H: lay.Height + 14}
	l.Shadow(r, c.Pal.Shadow, draw.Uniform(r.H/2), 8)
	l.BorderedRect(r, c.Pal.Surface, c.Pal.Border, 1, draw.Uniform(r.H/2))
	l.Rect(draw.Rect{X: r.X + 13, Y: r.Y + r.H/2 - 4, W: 8, H: 8}, dot, draw.Uniform(4))
	drawAt(l, lay, r.X+27, r.Y+7, c.Pal.Text)
}

// agentLine says what the whiteboard is doing, or "" before it has said.
func agentLine(a wire.AgentStatus) string {
	var s string
	switch a.State {
	case "":
		return ""
	case wire.AgentIdle:
		s = "Whiteboard ready"
	case wire.AgentListening:
		s = "Whiteboard listening"
	case wire.AgentWorking:
		s = "Whiteboard drawing"
	case wire.AgentSpeaking:
		s = "Whiteboard answering"
	case wire.AgentAuto:
		s = "Whiteboard diagramming as you talk"
	case wire.AgentOffline:
		s = "Whiteboard offline"
	default:
		s = "Whiteboard " + string(a.State)
	}
	if a.Detail != "" {
		s += " · " + a.Detail
	}
	if a.Queued > 0 {
		s += " · " + strconv.Itoa(a.Queued) + " waiting"
	}
	return s
}

func (s *meetingScene) event(ev *sdl.Event, pal theme.Palette) bool {
	if s.translate == nil {
		return false
	}
	uev, ok := s.translate(ev)
	if !ok {
		return false
	}
	c := s.ctx(pal, time.Now())
	if s.barEvent(uev) {
		return true
	}
	if uev.Kind == ui.KeyPress && uev.Mods == 0 && uev.Key == 'f' {
		s.view.Fit(c.Now)
		return true
	}
	return s.host.Event(c, uev).Redraw
}

func (s *meetingScene) animating() bool {
	return s.more || s.view.Animating(time.Now())
}

// woke takes what the connection published: a new version replays into the
// board (its new ops glide in), a whole new board resets it.
func (s *meetingScene) woke() {
	st, ok := s.mt.take(s.st.rev)
	if !ok {
		return
	}
	s.apply(st, time.Now())
}

func (s *meetingScene) apply(st meetingState, now time.Time) {
	changed := st.seq != s.st.seq
	s.st = st
	if !changed {
		return
	}
	doc, errs := board.Replay(st.ops)
	for _, err := range errs {
		slog.Warn("board: replay", "err", err)
	}
	if st.reveal != nil && len(st.ops) >= s.nops {
		s.view.SetDoc(doc, now) // grows: new boxes glide in
	} else {
		s.view.Reset(doc, now) // a whole board arrived (joining, an undo)
	}
	s.nops = len(st.ops)
}
