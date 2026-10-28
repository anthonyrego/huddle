package main

// The meeting window: the board stage filling it, a status pill at the top
// left (the connection and the whiteboard), the command field at the
// bottom of the stage, and the bar along the bottom (bar.go: the room, and
// the voice session from Track V).
//
// Talking to the whiteboard: say "whiteboard, …"; or type in the command
// field (/ focuses it, Enter sends, Esc leaves it); or hold the right ⌥
// key while you talk, which never types anything.

import (
	"log/slog"
	"strconv"
	"strings"
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

const (
	fieldMaxW  = 560
	fieldGap   = 14 // between the field and the bottom of the stage
	addressKey = sdl.K_RALT
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
	more   bool         // a widget asked for another frame

	field      *ui.TextField
	fieldRect  draw.Rect
	addressing bool // the push-to-address key is down

	// translate turns SDL events into ui events (the window's); send
	// queues a frame for the server (nil without a connection);
	// textInput turns the window's text input on and off.
	translate func(*sdl.Event) (ui.Event, bool)
	send      func(kind string, body any) bool
	textInput func(on bool)
}

func newMeetingScene(shaper *text.Shaper, scale func() float32, mt *meeting) *meetingScene {
	s := &meetingScene{shaper: shaper, scale: scale, mt: mt, view: boardview.New()}
	s.host.W = s.view
	s.view.OnClick = func(id string) { slog.Info("board: clicked", "id", id) }
	s.field = ui.NewTextField("Tell the whiteboard…   / to type · Enter sends · hold right ⌥ to talk to it", 1)
	s.field.MaxBytes = 600
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
	s.drawField(c, l, stage)
	s.drawBar(c, l, draw.Rect{Y: h - barH, W: w, H: barH})
}

// drawField draws the command field along the bottom of the stage, and
// a note over it while the push-to-address key is held.
func (s *meetingScene) drawField(c *ui.Ctx, l *draw.List, stage draw.Rect) {
	fw := min(fieldMaxW, stage.W-32)
	fh := s.field.Measure(c, fw)
	s.fieldRect = draw.Rect{X: stage.X + (stage.W-fw)/2, Y: stage.Bottom() - fh - fieldGap, W: fw, H: fh}
	l.Shadow(s.fieldRect, c.Pal.Shadow, draw.Uniform(6), 8)
	s.field.Draw(c, l, s.fieldRect)
	if s.addressing {
		lay := s.tc.lay(c, "Talking to the whiteboard…", text.SansSemibold, 12.5)
		r := draw.Rect{W: lay.Width + 28, H: lay.Height + 12}
		r.X, r.Y = s.fieldRect.X+(s.fieldRect.W-r.W)/2, s.fieldRect.Y-r.H-8
		l.Rect(r, c.Pal.Accent, draw.Uniform(r.H/2))
		drawAt(l, lay, r.X+14, r.Y+6, c.Pal.Background)
	}
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
		switch s.st.agent.State {
		case wire.AgentListening, wire.AgentSpeaking:
			dot = c.Pal.Accent
		case wire.AgentWorking, wire.AgentAuto:
			dot = c.Pal.Warning
		case wire.AgentOffline:
			dot = c.Pal.Error
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
		if a.Detail == "connecting" {
			s, a.Detail = "Whiteboard starting", ""
		}
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
	// The push-to-address key, on its own: it types nothing.
	if ev.Type == sdl.EVENT_KEY_DOWN || ev.Type == sdl.EVENT_KEY_UP {
		k := ev.KeyboardEvent()
		if k.Key == addressKey {
			if !k.Repeat {
				s.setAddressing(ev.Type == sdl.EVENT_KEY_DOWN)
			}
			return true
		}
	}
	if ev.Type == sdl.EVENT_WINDOW_FOCUS_LOST && s.addressing {
		s.setAddressing(false)
		return true
	}
	uev, ok := s.translate(ev)
	if !ok {
		return false
	}
	c := s.ctx(pal, time.Now())
	switch uev.Kind {
	case ui.PointerDown:
		if s.fieldRect.Contains(uev.Pos) {
			s.focusField(true)
			s.field.ClickAt(draw.Point{X: uev.Pos.X - s.fieldRect.X, Y: uev.Pos.Y - s.fieldRect.Y})
			return true
		}
		if s.field.Focused() {
			s.focusField(false)
		}
	case ui.KeyPress:
		if s.field.Focused() {
			switch {
			case uev.Key == ui.KeyEnter && uev.Mods&ui.Shift == 0:
				s.submit()
				return true
			case uev.Key == ui.KeyEscape:
				s.focusField(false)
				return true
			}
			res, act := s.field.Key(uev)
			if act == ui.FieldBlur {
				s.focusField(false)
			}
			return res.Redraw || res.Handled
		}
		if uev.Mods == 0 && uev.Key == '/' {
			s.focusField(true)
			return true
		}
		if uev.Mods == 0 && uev.Key == 'f' {
			s.view.Fit(c.Now)
			return true
		}
	case ui.KeyRelease:
		if s.field.Focused() {
			return false
		}
	case ui.TextInput:
		if s.field.Focused() {
			s.field.Input(uev.Text)
			return true
		}
		return false
	}
	return s.host.Event(c, uev).Redraw
}

func (s *meetingScene) focusField(on bool) {
	if s.field.Focused() == on {
		return
	}
	s.field.Focus(on)
	if s.textInput != nil {
		s.textInput(on)
	}
}

// submit sends the typed command.
func (s *meetingScene) submit() {
	t := strings.TrimSpace(s.field.Text())
	if t == "" {
		return
	}
	if s.send == nil || !s.send(wire.KindCommand, wire.Command{Room: s.st.room, Text: t}) {
		slog.Warn("command: not connected; kept in the field")
		return
	}
	slog.Info("command: typed", "chars", len(t))
	s.field.SetText("")
}

func (s *meetingScene) setAddressing(on bool) {
	if s.addressing == on {
		return
	}
	s.addressing = on
	if s.send != nil {
		s.send(wire.KindAddress, wire.Address{Room: s.st.room, Held: on})
	}
	slog.Info("command: address key", "held", on)
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

// apply shows what the connection heard: a newer version of the board
// shown glides there (at the server's placement), anything else (joining,
// a reconnect) shows the board at once; the ops that made a version order
// its new boxes' fade-in, and point_at highlights.
func (s *meetingScene) apply(st meetingState, now time.Time) {
	prev := s.st.seq
	changed := st.seq != prev
	s.st = st
	if changed {
		doc, errs := board.Replay(st.ops)
		for _, err := range errs {
			slog.Warn("board: replay", "err", err)
		}
		if prev > 0 && st.seq > prev {
			s.view.Show(doc, st.place, nil, now)
		} else {
			s.view.ResetTo(doc, st.place, now)
		}
	}
	if st.reveal != nil && st.revealSeq == st.seq {
		var ids []string
		for _, op := range st.reveal {
			switch op.Name {
			case board.AddNode, board.Frame:
				ids = append(ids, op.ID)
			case board.PointAt:
				ms := op.MS
				if ms <= 0 {
					ms = board.DefaultPointMS
				}
				s.view.Point(op.IDs, time.Duration(ms)*time.Millisecond, now)
			}
		}
		if len(ids) > 0 {
			s.view.Reveal(ids, now)
		}
	}
}
