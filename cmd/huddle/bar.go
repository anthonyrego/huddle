package main

// The bar along the bottom of the meeting window: the room and who you
// are, then the voice session: who's in the room (a dot lights while they
// talk), the mic, which m or a click mutes, and the ⌃ that opens the
// device menu (devmenu.go).

import (
	"strings"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/platform/audio"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

// barH is the bar's height.
const barH = 52

func (s *meetingScene) drawBar(c *ui.Ctx, l *draw.List, r draw.Rect) {
	l.Rect(r, c.Pal.Bar, draw.Radii{})
	l.Rect(draw.Rect{X: r.X, Y: r.Y, W: r.W, H: 1}, c.Pal.Border, draw.Radii{})
	room := s.st.room
	if room == "" {
		room = "No room"
	}
	lay := s.tc.lay(c, room, text.SansSemibold, 13)
	y := r.Y + (r.H-lay.Height)/2
	putFitAt(c, &s.tc, l, lay, r.X+20, y, r.W*0.25, c.Pal.Text, text.SansSemibold, 13)
	x := r.X + 20 + min(lay.Width, r.W*0.25) + 12
	sub := ""
	switch {
	case connDown(s.st.conn) && s.st.server != "":
		sub = "server " + s.st.server
	case s.call != nil:
	case s.st.identity != "":
		sub = "you are " + s.st.identity
	case s.st.conn == connNoRoom:
		sub = "join one with: " + s.st.detail
	}
	if sub != "" {
		who := s.tc.lay(c, sub, text.SansRegular, 12)
		drawAt(l, who, x, r.Y+(r.H-who.Height)/2, c.Pal.TextMuted)
		x += who.Width + 14
	}
	right := r.X + r.W - 20
	hint := s.tc.lay(c, "f  fit", text.SansRegular, 11.5)
	right -= hint.Width
	drawAt(l, hint, right, r.Y+(r.H-hint.Height)/2, c.Pal.TextFaint)
	s.muteRect, s.devRect = draw.Rect{}, draw.Rect{}
	if s.call == nil {
		return
	}
	v := s.call.snapshot()
	right = s.drawDevices(c, l, right-16, r)
	right = s.drawMic(c, l, v, right-6, r)
	s.drawPeople(c, l, v, x, right-12, r)
}

// drawDevices draws the button that opens the device menu, ending at
// right, and returns where it starts.
func (s *meetingScene) drawDevices(c *ui.Ctx, l *draw.List, right float32, r draw.Rect) float32 {
	h := float32(28)
	b := draw.Rect{X: right - h, Y: r.Y + (r.H-h)/2, W: h, H: h}
	fill, stroke, col := c.Pal.Surface, c.Pal.Border, c.Pal.TextMuted
	if s.menu.open {
		fill, stroke, col = c.Pal.SurfaceAlt, c.Pal.Accent, c.Pal.Accent
	}
	l.BorderedRect(b, fill, stroke, 1, draw.Uniform(h/2))
	cx, cy := b.X+h/2, b.Y+h/2
	l.Path(draw.Path{Points: []draw.Point{{X: cx - 4.5, Y: cy + 2}, {X: cx, Y: cy - 2.5}, {X: cx + 4.5, Y: cy + 2}}, Stroke: col, Width: 1.6})
	s.devRect = b
	return b.X
}

// drawMic draws the mic control ending at right and returns where it
// starts.
func (s *meetingScene) drawMic(c *ui.Ctx, l *draw.List, v callState, right float32, r draw.Rect) float32 {
	label := v.status
	if v.noEcho && v.status == callOn {
		label = "Mic on · use headphones"
	}
	if v.status == callRetry && v.retryIn > 0 {
		label += " · retrying in " + itoa(v.retryIn) + " s"
	}
	if v.canMute {
		label += "  ·  m"
	}
	lay := s.tc.lay(c, label, text.SansSemibold, 12)
	h := float32(28)
	pill := draw.Rect{X: right - lay.Width - 34, Y: r.Y + (r.H-h)/2, W: lay.Width + 34, H: h}
	fill, dot, col := c.Pal.Surface, c.Pal.TextFaint, c.Pal.Text
	switch v.status {
	case callOn:
		dot = c.Pal.Success
		if v.noEcho {
			dot = c.Pal.Warning
		}
		if v.talking[v.self] {
			dot = c.Pal.Accent
		}
	case callMuted:
		dot, col = c.Pal.Error, c.Pal.Error
	case callLost, callRetry:
		dot, col = c.Pal.Error, c.Pal.TextMuted
	case callReconnecting:
		dot, col = c.Pal.Warning, c.Pal.TextMuted
	case callListening, callJoining, callSwitching, callWaiting:
		col = c.Pal.TextMuted
	}
	l.BorderedRect(pill, fill, c.Pal.Border, 1, draw.Uniform(h/2))
	l.Rect(draw.Rect{X: pill.X + 12, Y: pill.Y + h/2 - 4, W: 8, H: 8}, dot, draw.Uniform(4))
	drawAt(l, lay, pill.X+26, pill.Y+(h-lay.Height)/2, col)
	if v.canMute {
		s.muteRect = pill
	}
	return pill.X
}

// drawPeople draws a chip per person in the room, you first, between x and
// right; those that don't fit become "+N".
func (s *meetingScene) drawPeople(c *ui.Ctx, l *draw.List, v callState, x, right float32, r draw.Rect) {
	type chip struct {
		name    string
		talking bool
		agent   bool
	}
	var chips []chip
	if v.self != "" {
		chips = append(chips, chip{name: "You", talking: v.talking[v.self] && !v.muted})
	}
	for _, p := range v.people {
		chips = append(chips, chip{name: displayName(p), talking: v.talking[p.Identity], agent: p.Agent})
	}
	h := float32(26)
	for i, ch := range chips {
		lay := s.tc.lay(c, ch.name, text.SansRegular, 12)
		w := lay.Width + 30
		more := len(chips) - i - 1
		reserve := float32(0)
		if more > 0 {
			reserve = 40
		}
		if x+w+reserve > right {
			rest := s.tc.lay(c, "+"+itoa(len(chips)-i), text.SansSemibold, 12)
			if x+rest.Width <= right {
				drawAt(l, rest, x, r.Y+(r.H-rest.Height)/2, c.Pal.TextMuted)
			}
			return
		}
		box := draw.Rect{X: x, Y: r.Y + (r.H-h)/2, W: w, H: h}
		stroke, dot := c.Pal.Border, c.Pal.TextFaint
		if ch.talking {
			stroke, dot = c.Pal.Accent, c.Pal.Accent
		}
		l.BorderedRect(box, c.Pal.Surface, stroke, 1, draw.Uniform(h/2))
		if ch.agent {
			l.Rect(draw.Rect{X: box.X + 10, Y: box.Y + h/2 - 4, W: 8, H: 8}, dot, draw.Uniform(2))
		} else {
			l.Rect(draw.Rect{X: box.X + 10, Y: box.Y + h/2 - 4, W: 8, H: 8}, dot, draw.Uniform(4))
		}
		drawAt(l, lay, box.X+22, box.Y+(h-lay.Height)/2, c.Pal.Text)
		x += w + 6
	}
}

// displayName is how a person is shown: their name, else their identity
// without the dev join's suffix.
func displayName(p rtc.Participant) string {
	if p.Name != "" {
		return p.Name
	}
	if i := strings.LastIndexByte(p.Identity, '-'); i > 0 {
		return p.Identity[:i]
	}
	return p.Identity
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// barEvent handles the bar's input: m, or a click on the mic, mutes; d, or
// a click on the ⌃ beside it, opens the device menu.
func (s *meetingScene) barEvent(e ui.Event) bool {
	if s.call == nil {
		return false
	}
	switch {
	case e.Kind == ui.KeyPress && e.Mods == 0 && e.Key == 'm' && !e.Repeat:
		return s.call.toggleMute()
	case e.Kind == ui.PointerDown && e.Button == 1 && s.muteRect.W > 0 && s.muteRect.Contains(e.Pos):
		return s.call.toggleMute()
	case e.Kind == ui.KeyPress && e.Mods == 0 && e.Key == 'd' && !e.Repeat,
		e.Kind == ui.PointerDown && e.Button == 1 && s.devRect.W > 0 && s.devRect.Contains(e.Pos):
		s.openMenu(audio.VoiceDevices())
		return true
	}
	return false
}
