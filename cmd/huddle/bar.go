package main

// The bar along the bottom of the meeting window: the room and who you
// are. Track V adds the voice session here: who's in the room, who's
// talking, and the mic.

import (
	"github.com/anthonyrego/huddle/internal/draw"
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
	putFitAt(c, &s.tc, l, lay, r.X+20, y, r.W*0.5, c.Pal.Text, text.SansSemibold, 13)
	sub := ""
	switch {
	case s.st.identity != "":
		sub = "you are " + s.st.identity
	case s.st.conn == connNoRoom:
		sub = "join one with: " + s.st.detail
	}
	if sub != "" {
		who := s.tc.lay(c, sub, text.SansRegular, 12)
		drawAt(l, who, r.X+20+min(lay.Width, r.W*0.5)+12, r.Y+(r.H-who.Height)/2, c.Pal.TextMuted)
	}
	hint := s.tc.lay(c, "f  fit", text.SansRegular, 11.5)
	drawAt(l, hint, r.X+r.W-hint.Width-20, r.Y+(r.H-hint.Height)/2, c.Pal.TextFaint)
}
