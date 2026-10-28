package main

// The device menu: the ⌃ beside the mic pill (or d) opens it above the
// bar, listing the microphones, the speakers, echo cancelling and the
// cameras with the current ones checked. A pick switches at once and
// closes the menu: a mic or speakers through call.pick, a camera through
// pickCamera, which leaves the voice's devices alone. Its look follows
// reel's dropdown (github.com/anthonyrego/reel, internal/ui/controls
// Menu): rows in a shadowed card, a check on the current one, the wheel
// scrolling what doesn't fit, and a click anywhere else closing it. The
// lists are read when the menu opens, so a device plugged in since shows
// up.

import (
	"log/slog"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/platform/audio"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

type rowKind int

const (
	rowHeader rowKind = iota // a section's title
	rowChoice                // a device or a setting: picks
	rowNote                  // something to know, not a choice
)

type menuRow struct {
	kind   rowKind
	label  string
	detail string      // muted, after the label
	on     bool        // the current choice
	pick   devicePrefs // what choosing it picks
	camera bool        // a camera: picking it changes only pick.Camera
}

type devMenu struct {
	open      bool
	rows      []menuRow
	rect      draw.Rect // where it was drawn
	hits      []menuHit
	hover     int // the row under the pointer, -1 none
	scroll    float32
	maxScroll float32
}

type menuHit struct {
	r   draw.Rect
	row int
}

// deviceRows is the menu for the devices listed (listErr when they
// couldn't be), what is picked now and what the call says about them.
// Without a mic (-no-voice) only the speakers are offered.
func deviceRows(devs []audio.VoiceDevice, listErr error, p devicePrefs, st callState, mic bool) []menuRow {
	var rows []menuRow
	if listErr != nil {
		rows = append(rows, menuRow{kind: rowNote, label: "Can't list the devices: " + listErr.Error()})
	}
	section := func(title string, input bool, cur string, set func(*devicePrefs, string)) {
		rows = append(rows, menuRow{kind: rowHeader, label: title})
		def := ""
		for _, d := range devs {
			if input && d.DefaultIn || !input && d.DefaultOut {
				def = d.Name
			}
		}
		q := p
		set(&q, "")
		rows = append(rows, menuRow{kind: rowChoice, label: "System default", detail: def, on: cur == "", pick: q})
		found := cur == ""
		for _, d := range devs {
			if input && d.In == 0 || !input && d.Out == 0 {
				continue
			}
			q := p
			set(&q, d.UID)
			rows = append(rows, menuRow{kind: rowChoice, label: d.Name, detail: d.Transport, on: cur == d.UID, pick: q})
			found = found || cur == d.UID
		}
		if !found {
			rows = append(rows, menuRow{kind: rowNote, label: "The one picked isn't plugged in: using the default"})
		}
	}
	if mic {
		section("Microphone", true, p.Mic, func(q *devicePrefs, uid string) { q.Mic = uid })
	}
	section("Speakers", false, p.Speakers, func(q *devicePrefs, uid string) { q.Speakers = uid })
	if mic {
		on, off := p, p
		on.EchoOff, off.EchoOff = false, true
		rows = append(rows,
			menuRow{kind: rowHeader, label: "Echo cancelling"},
			menuRow{kind: rowChoice, label: "On", detail: "for speakers", on: !p.EchoOff, pick: on},
			menuRow{kind: rowChoice, label: "Off", detail: "for headphones", on: p.EchoOff, pick: off})
		if st.noEcho {
			rows = append(rows, menuRow{kind: rowNote, label: "It won't start on these devices: use headphones"})
		}
		if st.detail != "" {
			rows = append(rows, menuRow{kind: rowNote, label: "No mic: " + st.detail})
		}
	}
	if st.lostErr != "" {
		rows = append(rows, menuRow{kind: rowNote, label: "Voice: " + st.lostErr})
	}
	return rows
}

// cameraRows is the menu's Camera section: the cameras listed (listErr
// when they couldn't be), the one picked checked, and why the camera
// isn't on if it failed (the call's word, else the button's ask's).
func cameraRows(cams []cameraDevice, listErr error, p devicePrefs, v videoState, askErr string) []menuRow {
	rows := []menuRow{{kind: rowHeader, label: "Camera"}}
	if listErr != nil {
		rows = append(rows, menuRow{kind: rowNote, label: "Can't list the cameras: " + listErr.Error()})
	} else {
		def := ""
		for _, d := range cams {
			if d.Default {
				def = d.Name
			}
		}
		q := p
		q.Camera = ""
		rows = append(rows, menuRow{kind: rowChoice, label: "System default", detail: def, on: p.Camera == "", pick: q, camera: true})
		found := p.Camera == ""
		for _, d := range cams {
			q := p
			q.Camera = d.UID
			rows = append(rows, menuRow{kind: rowChoice, label: d.Name, detail: d.Transport, on: p.Camera == d.UID, pick: q, camera: true})
			found = found || p.Camera == d.UID
		}
		switch {
		case len(cams) == 0:
			rows = append(rows, menuRow{kind: rowNote, label: "No camera found"})
		case !found:
			rows = append(rows, menuRow{kind: rowNote, label: "The one picked isn't plugged in: using the default"})
		}
	}
	err := v.camErr
	if err == "" {
		err = askErr
	}
	if err != "" && !v.camera {
		rows = append(rows, menuRow{kind: rowNote, label: "Camera: " + err})
	}
	return rows
}

// pickCamera keeps the camera picked (in devices.json, with the other
// picks) and, while your camera is on, asks for it again so it moves to
// the one picked. The mic and speakers aren't touched.
func (s *meetingScene) pickCamera(uid string) {
	c := s.call
	c.mu.Lock()
	c.prefs.Camera = uid
	p, on := c.prefs, c.vid.camera
	c.mu.Unlock()
	if c.data != "" {
		if err := writeDevicePrefs(c.data, p); err != nil {
			slog.Warn("devices: couldn't keep the choice", "err", err)
		}
	}
	slog.Info("devices: picked", "camera", uid != "")
	if on {
		s.askCamera(true)
	}
}

// Menu layout.
const (
	menuRowH  = 30
	menuHeadH = 26
	menuMinW  = 280
	menuMaxW  = 440
)

func rowHeight(k rowKind) float32 {
	if k == rowHeader {
		return menuHeadH
	}
	return menuRowH
}

// openMenu opens the device menu on the devices and cameras listed.
func (s *meetingScene) openMenu(devs []audio.VoiceDevice, err error, cams []cameraDevice, camErr error) {
	if s.call == nil {
		return
	}
	p := s.call.devicePicks()
	_, _, askErr, _ := s.asks.get()
	rows := deviceRows(devs, err, p, s.call.snapshot(), !s.call.noVoice)
	rows = append(rows, cameraRows(cams, camErr, p, s.call.videoSnapshot(), askErr)...)
	s.menu = devMenu{open: true, hover: -1, rows: rows}
}

// drawMenu draws the open menu above the device button, in a w×h window.
func (s *meetingScene) drawMenu(c *ui.Ctx, l *draw.List, w float32) {
	m := &s.menu
	m.hits = m.hits[:0]
	if !m.open || s.devRect.W == 0 {
		return
	}
	pal := c.Pal
	width, total := float32(menuMinW), float32(8)
	for _, r := range m.rows {
		total += rowHeight(r.kind)
		cw := 28 + s.tc.lay(c, r.label, text.SansRegular, 13).Width + 16
		if r.detail != "" {
			cw += 10 + s.tc.lay(c, r.detail, text.SansRegular, 12).Width
		}
		width = max(width, min(cw+8, menuMaxW))
	}
	width = min(width, w-16)
	anchor := s.devRect
	gap := float32(8) + (barH-anchor.H)/2 // clear of the bar, not just the button
	ht := min(total, anchor.Y-gap-8)
	r := draw.Rect{X: max(8, min(anchor.Right()-width, w-8-width)), Y: anchor.Y - gap - ht, W: width, H: ht}
	m.rect = r
	m.maxScroll = max(0, total-ht)
	m.scroll = max(0, min(m.scroll, m.maxScroll))
	l.Shadow(r, pal.Shadow, draw.Uniform(10), 18)
	l.BorderedRect(r, pal.Surface.WithAlpha(0xff), pal.Border, 1, draw.Uniform(10))
	inner := draw.Rect{X: r.X, Y: r.Y + 4, W: r.W, H: r.H - 8}
	l.PushClip(inner)
	defer l.PopClip()
	y := inner.Y - m.scroll
	for i, row := range m.rows {
		rr := draw.Rect{X: r.X + 4, Y: y, W: r.W - 8, H: rowHeight(row.kind)}
		y += rr.H
		if rr.Bottom() <= inner.Y || rr.Y >= inner.Bottom() {
			continue
		}
		switch row.kind {
		case rowHeader:
			lay := s.tc.lay(c, row.label, text.SansSemibold, 11)
			drawAt(l, lay, rr.X+12, rr.Bottom()-lay.Height-4, pal.TextMuted)
		case rowNote:
			lay := s.tc.lay(c, row.label, text.SansRegular, 12)
			putFitAt(c, &s.tc, l, lay, rr.X+12, rr.Y+(rr.H-lay.Height)/2, rr.W-24, pal.Warning, text.SansRegular, 12)
		case rowChoice:
			m.hits = append(m.hits, menuHit{r: rr.Intersect(inner), row: i})
			if i == m.hover {
				l.Rect(rr, pal.SurfaceAlt, draw.Uniform(6))
			}
			if row.on {
				check(l, rr.X+14, rr.Y+rr.H/2, pal.Accent)
			}
			lay := s.tc.lay(c, row.label, text.SansRegular, 13)
			ty := rr.Y + (rr.H-lay.Height)/2
			putFitAt(c, &s.tc, l, lay, rr.X+28, ty, rr.W-40, pal.Text, text.SansRegular, 13)
			if row.detail != "" && lay.Width+40 < rr.W-40 {
				det := s.tc.lay(c, row.detail, text.SansRegular, 12)
				putFitAt(c, &s.tc, l, det, rr.X+28+lay.Width+10, ty+1, rr.W-lay.Width-50, pal.TextMuted, text.SansRegular, 12)
			}
		}
	}
}

// check draws a small check mark centered at x, y.
func check(l *draw.List, x, y float32, col draw.Color) {
	l.Path(draw.Path{
		Points: []draw.Point{{X: x - 4, Y: y}, {X: x - 1, Y: y + 3}, {X: x + 5, Y: y - 4}},
		Stroke: col, Width: 1.75,
	})
}

// menuEvent handles input while the menu is open; it takes all of it.
func (s *meetingScene) menuEvent(e ui.Event) bool {
	m := &s.menu
	switch e.Kind {
	case ui.KeyPress:
		if e.Key == ui.KeyEscape || e.Mods == 0 && e.Key == 'd' {
			m.open = false
		}
		return true
	case ui.PointerMove:
		hover := -1
		for _, h := range m.hits {
			if h.r.Contains(e.Pos) {
				hover = h.row
			}
		}
		changed := hover != m.hover
		m.hover = hover
		return changed
	case ui.Wheel:
		if m.rect.Contains(e.Pos) {
			m.scroll = max(0, min(m.scroll+e.DY, m.maxScroll))
		}
		return true
	case ui.PointerDown:
		for _, h := range m.hits {
			if h.r.Contains(e.Pos) {
				m.open = false
				if row := m.rows[h.row]; row.camera {
					s.pickCamera(row.pick.Camera)
				} else {
					s.call.pick(row.pick)
				}
				return true
			}
		}
		if !m.rect.Contains(e.Pos) {
			m.open = false // a click elsewhere only closes it
		}
		return true
	}
	return false
}

// renderDevices are made-up devices for render -devices.
var renderDevices = []audio.VoiceDevice{
	{UID: "iface", Name: "Studio Interface", Transport: "USB", In: 2, Out: 2, Rate: 44100, DefaultIn: true, DefaultOut: true},
	{UID: "mic", Name: "Built-in Microphone", Transport: "built-in", In: 1, Rate: 48000},
	{UID: "speakers", Name: "Built-in Speakers", Transport: "built-in", Out: 2, Rate: 48000},
	{UID: "monitor", Name: "Desk Monitor", Transport: "HDMI", Out: 2, Rate: 48000},
	{UID: "headset", Name: "Wireless Headset", Transport: "Bluetooth", In: 1, Out: 2, Rate: 24000},
	{UID: "capture", Name: "Capture Card", Transport: "USB", In: 2, Rate: 48000},
}
