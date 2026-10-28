package main

// The bar along the bottom of the meeting window: the room and who you
// are, then the voice session: who's in the room (a dot lights while they
// talk), the Share and Camera buttons, the mic, which m or a click mutes,
// and the ⌃ that opens the device menu (devmenu.go). The Share and Camera
// buttons say what's on (or why it isn't: the error, cut to fit; the
// device menu has all of it) and turn it on or off on a goroutine, since
// a camera can take a while to start and the share waits on the system's
// picker. In a narrow window they're icons alone. They have no keys: a
// stray letter shouldn't turn a camera on or show a screen.

import (
	"log/slog"
	"strings"
	"sync"

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
	s.muteRect, s.devRect, s.camRect, s.shareRect = draw.Rect{}, draw.Rect{}, draw.Rect{}, draw.Rect{}
	if s.call == nil {
		return
	}
	v := s.call.snapshot()
	vs := s.call.videoSnapshot()
	compact := r.W < compactBarW
	right = s.drawDevices(c, l, right-16, r)
	right = s.drawMic(c, l, v, right-6, r)
	right = s.drawCamera(c, l, vs, right-6, r, compact)
	right = s.drawShareButton(c, l, vs, right-6, r, compact)
	s.drawPeople(c, l, v, x, right-12, r)
}

// compactBarW is the bar's width below which the Share and Camera buttons
// are icons alone.
const compactBarW = 860

// videoAsks is what the Camera and Share buttons asked for and heard
// back. The asks run on goroutines, which write here under the lock and
// wake the loop; the bar reads it on the main thread.
type videoAsks struct {
	mu        sync.Mutex
	camBusy   bool   // a camera ask is running
	shareBusy bool   // a share ask is running (the system's picker may be up)
	camErr    string // why the last camera ask failed ("" it didn't)
	shareErr  string
}

func (a *videoAsks) get() (camBusy, shareBusy bool, camErr, shareErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.camBusy, a.shareBusy, a.camErr, a.shareErr
}

// askCamera turns your camera on or off on a goroutine; nothing happens
// while an ask is running.
func (s *meetingScene) askCamera(on bool) {
	s.ask(on, "camera", &s.asks.camBusy, &s.asks.camErr, s.call.setCamera)
}

// askShare starts sharing (the system's picker chooses what) or stops, on
// a goroutine.
func (s *meetingScene) askShare(on bool) {
	s.ask(on, "share", &s.asks.shareBusy, &s.asks.shareErr, s.call.setShare)
}

func (s *meetingScene) ask(on bool, what string, busy *bool, errText *string, set func(bool) error) {
	a, cl := &s.asks, s.call
	a.mu.Lock()
	if *busy {
		a.mu.Unlock()
		return
	}
	*busy = true
	a.mu.Unlock()
	go func() {
		err := set(on)
		a.mu.Lock()
		*busy, *errText = false, ""
		if err != nil {
			*errText = err.Error()
		}
		a.mu.Unlock()
		if err != nil {
			slog.Warn("video: "+what, "on", on, "err", err)
		} else {
			slog.Info("video: "+what, "on", on)
		}
		if cl.wake != nil {
			cl.wake()
		}
	}()
}

// buttonLook is how a Share or Camera button looks now.
type buttonLook struct {
	label  string
	col    draw.Color // the icon and the label
	stroke draw.Color // the border
	off    bool       // the icon crossed out
	alert  bool       // an error (a dot on the icon alone)
}

// cameraLook is the Camera button for v and the asks.
func (s *meetingScene) cameraLook(c *ui.Ctx, v videoState) buttonLook {
	busy, _, askErr, _ := s.asks.get()
	err := v.camErr
	if err == "" {
		err = askErr
	}
	pal := c.Pal
	switch {
	case busy:
		return buttonLook{label: "Camera…", col: pal.TextMuted, stroke: pal.Border}
	case v.camera:
		return buttonLook{label: "Camera on", col: pal.Success, stroke: pal.Border}
	case err != "":
		return buttonLook{label: "Camera: " + err, col: pal.Error, stroke: pal.Border, off: true, alert: true}
	}
	return buttonLook{label: "Camera off", col: pal.TextMuted, stroke: pal.Border, off: true}
}

// shareLook is the Share button for v and the asks.
func (s *meetingScene) shareLook(c *ui.Ctx, v videoState) buttonLook {
	_, busy, _, askErr := s.asks.get()
	err := v.shareEr
	if err == "" {
		err = askErr
	}
	pal := c.Pal
	switch {
	case busy:
		return buttonLook{label: "Choosing…", col: pal.TextMuted, stroke: pal.Border}
	case v.sharing:
		return buttonLook{label: "Sharing · stop", col: pal.Accent, stroke: pal.Accent}
	case err != "":
		return buttonLook{label: "Share: " + err, col: pal.Error, stroke: pal.Border, alert: true}
	}
	return buttonLook{label: "Share screen", col: pal.TextMuted, stroke: pal.Border}
}

// drawCamera draws the Camera button ending at right and returns where
// it starts.
func (s *meetingScene) drawCamera(c *ui.Ctx, l *draw.List, v videoState, right float32, r draw.Rect, compact bool) float32 {
	s.camRect = s.drawButton(c, l, s.cameraLook(c, v), cameraIcon, right, r, compact)
	return s.camRect.X
}

// drawShareButton draws the Share button ending at right and returns
// where it starts.
func (s *meetingScene) drawShareButton(c *ui.Ctx, l *draw.List, v videoState, right float32, r draw.Rect, compact bool) float32 {
	s.shareRect = s.drawButton(c, l, s.shareLook(c, v), shareIcon, right, r, compact)
	return s.shareRect.X
}

// drawButton draws a bar button ending at right: its icon, and its label
// unless compact.
func (s *meetingScene) drawButton(c *ui.Ctx, l *draw.List, b buttonLook, icon func(*draw.List, float32, float32, draw.Color, bool), right float32, r draw.Rect, compact bool) draw.Rect {
	const (
		h        = 28
		maxLabel = 210
	)
	pal := c.Pal
	var box draw.Rect
	if compact {
		box = draw.Rect{X: right - h, Y: r.Y + (r.H-h)/2, W: h, H: h}
		l.BorderedRect(box, pal.Surface, b.stroke, 1, draw.Uniform(h/2))
		icon(l, box.X+h/2, box.Y+h/2, b.col, b.off)
		if b.alert {
			l.Rect(draw.Rect{X: box.Right() - 9, Y: box.Y + 2, W: 7, H: 7}, pal.Error, draw.Uniform(3.5))
		}
		return box
	}
	lay := s.tc.lay(c, b.label, text.SansSemibold, 12)
	lw := min(lay.Width, maxLabel)
	box = draw.Rect{X: right - lw - 42, Y: r.Y + (r.H-h)/2, W: lw + 42, H: h}
	l.BorderedRect(box, pal.Surface, b.stroke, 1, draw.Uniform(h/2))
	icon(l, box.X+18, box.Y+h/2, b.col, b.off)
	col := pal.Text
	if b.alert || b.col == pal.TextMuted {
		col = b.col
	}
	putFitAt(c, &s.tc, l, lay, box.X+30, box.Y+(h-lay.Height)/2, maxLabel, col, text.SansSemibold, 12)
	return box
}

// cameraIcon draws a small camera centered at x, y; off crosses it out.
func cameraIcon(l *draw.List, x, y float32, col draw.Color, off bool) {
	l.BorderedRect(draw.Rect{X: x - 7, Y: y - 4, W: 9.5, H: 8}, col.WithAlpha(0), col, 1.3, draw.Uniform(2))
	l.Path(draw.Path{Points: []draw.Point{{X: x + 3.5, Y: y - 1}, {X: x + 7, Y: y - 3.5}, {X: x + 7, Y: y + 3.5}, {X: x + 3.5, Y: y + 1}}, Closed: true, Fill: col})
	if off {
		l.Path(draw.Path{Points: []draw.Point{{X: x - 7, Y: y - 6}, {X: x + 7, Y: y + 6}}, Stroke: col, Width: 1.3})
	}
}

// shareIcon draws a small screen with an arrow up out of it, centered at
// x, y.
func shareIcon(l *draw.List, x, y float32, col draw.Color, _ bool) {
	l.BorderedRect(draw.Rect{X: x - 7, Y: y - 5, W: 14, H: 10}, col.WithAlpha(0), col, 1.3, draw.Uniform(2))
	l.Path(draw.Path{Points: []draw.Point{{X: x, Y: y + 2.5}, {X: x, Y: y - 2.5}}, Stroke: col, Width: 1.3})
	l.Path(draw.Path{Points: []draw.Point{{X: x - 2.5, Y: y}, {X: x, Y: y - 2.6}, {X: x + 2.5, Y: y}}, Stroke: col, Width: 1.3})
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
		devs, err := audio.VoiceDevices()
		cams, camErr := cameras()
		s.openMenu(devs, err, cams, camErr)
		return true
	case e.Kind == ui.PointerDown && e.Button == 1 && s.camRect.W > 0 && s.camRect.Contains(e.Pos):
		s.askCamera(!s.call.videoSnapshot().camera)
		return true
	case e.Kind == ui.PointerDown && e.Button == 1 && s.shareRect.W > 0 && s.shareRect.Contains(e.Pos):
		s.askShare(!s.call.videoSnapshot().sharing)
		return true
	}
	return false
}
