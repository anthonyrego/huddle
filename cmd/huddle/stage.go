package main

// The stage: the whiteboard, or a screen someone shares, in what the
// camera tiles leave of the window above the bar (tiles.go). While anyone
// shares, tabs at the top of the stage switch between them: "Whiteboard |
// Ann's screen", a tab for each share. A share that starts takes the
// stage, and when the one on stage ends the board comes back. While a
// share is on stage, a new version of the board puts a dot on the
// Whiteboard tab until the board is looked at. The status pill and the
// command field stay where they are whichever is on stage.

import (
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

// shareTab is a screen being shared, as its tab.
type shareTab struct {
	id    string // whose it is
	label string
	track rtc.VideoTrack
}

// tabHit is where a tab was drawn, for clicks.
type tabHit struct {
	r  draw.Rect
	id string // "" the board
}

// screenTabs lists the screens being shared, in the order they came.
func screenTabs(st callState, v videoState) []shareTab {
	var tabs []shareTab
	for _, t := range v.tracks {
		if t.Kind() != rtc.Screen {
			continue
		}
		name := displayName(rtc.Participant{Identity: t.Identity()})
		for _, p := range st.people {
			if p.Identity == t.Identity() {
				name = displayName(p)
			}
		}
		tabs = append(tabs, shareTab{id: t.Identity(), label: name + "'s screen", track: t})
	}
	return tabs
}

// followShares keeps the stage on what's shared now: a share that's new
// takes the stage, and when the one on stage ends, the board comes back.
func (s *meetingScene) followShares(tabs []shareTab) {
	now := map[string]bool{}
	for _, t := range tabs {
		now[t.id] = true
		if !s.shares[t.id] {
			s.onStage = t.id
		}
	}
	if s.onStage != "" && !now[s.onStage] {
		s.onStage = ""
	}
	s.shares = now
}

// onStageTab is the share on stage, or nil for the board.
func (s *meetingScene) onStageTab(tabs []shareTab) *shareTab {
	for i := range tabs {
		if tabs[i].id == s.onStage {
			return &tabs[i]
		}
	}
	return nil
}

// boardBadge is whether the board has changed since it was last on stage.
func (s *meetingScene) boardBadge() bool { return s.onStage != "" && s.st.seq > s.boardSeen }

// The tabs' measures.
const (
	tabInset  = 3  // between the strip's edge and the chosen tab
	tabPadX   = 12 // either side of a label
	tabBadge  = 7
	tabMaxLab = 180
	// pillMinW is the least the status pill keeps beside the tabs; with
	// less room they go under it.
	pillMinW = 150
)

// tabSeg is one tab, measured.
type tabSeg struct {
	id    string
	lay   text.Layout
	w     float32
	badge bool
}

// measureTabs measures the tabs while anything is shared, and the width
// the strip may take: with room for the board's badge whether it shows or
// not, so what shares the row with the status pill never changes with it.
func (s *meetingScene) measureTabs(c *ui.Ctx, stage draw.Rect, tabs []shareTab) ([]tabSeg, float32) {
	if len(tabs) == 0 {
		return nil, 0
	}
	segs := []tabSeg{{id: "", lay: s.tc.lay(c, "Whiteboard", text.SansSemibold, 12.5), badge: s.boardBadge()}}
	for _, t := range tabs {
		segs = append(segs, tabSeg{id: t.id, lay: s.tc.lay(c, t.label, text.SansSemibold, 12.5)})
	}
	most := float32(2*tabInset + tabBadge + 6)
	for i := range segs {
		segs[i].w = min(segs[i].lay.Width, tabMaxLab) + 2*tabPadX
		most += segs[i].w
		if segs[i].badge {
			segs[i].w += tabBadge + 6
		}
	}
	return segs, min(most, stage.W-32)
}

// drawTabs draws the tabs (segs) at the top of the stage, beside the
// status pill (pill) or under it, and returns where (empty: no tabs).
func (s *meetingScene) drawTabs(c *ui.Ctx, l *draw.List, stage, pill draw.Rect, segs []tabSeg, under bool) draw.Rect {
	s.tabHits = s.tabHits[:0]
	if len(segs) == 0 {
		return draw.Rect{}
	}
	pal := c.Pal
	total := float32(2 * tabInset)
	for _, sg := range segs {
		total += sg.w
	}
	total = min(total, stage.W-32)
	r := draw.Rect{X: stage.X + (stage.W-total)/2, Y: pill.Y, W: total, H: pill.H}
	if r.X < pill.Right()+10 {
		r.X = pill.Right() + 10
	}
	if under {
		r.X, r.Y = stage.X+16, pill.Bottom()+8
	}
	l.Shadow(r, pal.Shadow, draw.Uniform(r.H/2), 8)
	l.BorderedRect(r, pal.Surface, pal.Border, 1, draw.Uniform(r.H/2))
	x := r.X + tabInset
	for i, sg := range segs {
		w := sg.w
		if i == len(segs)-1 {
			w = r.Right() - tabInset - x // the last takes what's left, when squeezed
		}
		tr := draw.Rect{X: x, Y: r.Y + tabInset, W: w, H: r.H - 2*tabInset}
		col := pal.TextMuted
		if sg.id == s.onStage {
			l.BorderedRect(tr, pal.SurfaceAlt, pal.Border, 1, draw.Uniform(tr.H/2))
			col = pal.Text
		}
		room := tr.W - 2*tabPadX
		if sg.badge {
			room -= tabBadge + 6
		}
		ty := tr.Y + (tr.H-sg.lay.Height)/2
		putFitAt(c, &s.tc, l, sg.lay, tr.X+tabPadX, ty, room, col, text.SansSemibold, 12.5)
		if sg.badge {
			bx := tr.X + tabPadX + min(sg.lay.Width, room) + 6
			l.Rect(draw.Rect{X: bx, Y: tr.Y + tr.H/2 - tabBadge/2, W: tabBadge, H: tabBadge}, pal.Accent, draw.Uniform(tabBadge/2))
		}
		s.tabHits = append(s.tabHits, tabHit{r: tr, id: sg.id})
		x += w
	}
	return r
}

// drawShare draws the shared screen on stage, as large as fits in area
// with its own shape, and tells its track that size.
func (s *meetingScene) drawShare(c *ui.Ctx, l *draw.List, area draw.Rect, t shareTab) {
	if area.W <= 0 || area.H <= 0 {
		return
	}
	pal := c.Pal
	key := videoKey(t.track)
	pic, has := s.vid.take(key, t.track, 0, false)
	if !has {
		s.vid.ask(t.track, key, area, c.Scale)
		lay := s.tc.lay(c, "Waiting for "+t.label+"…", text.SansRegular, 14)
		drawAt(l, lay, area.X+(area.W-lay.Width)/2, area.Y+(area.H-lay.Height)/2, pal.TextFaint)
		return
	}
	k := min(area.W/float32(pic.fw), area.H/float32(pic.fh))
	w, h := float32(pic.fw)*k, float32(pic.fh)*k
	r := draw.Rect{X: area.X + (area.W-w)/2, Y: area.Y + (area.H-h)/2, W: w, H: h}
	s.vid.ask(t.track, key, r, c.Scale)
	l.Shadow(r, pal.Shadow, draw.Uniform(8), 12)
	l.Image(r, pic.id, draw.Uniform(8), 255)
	l.BorderedRect(r, pal.Border.WithAlpha(0), pal.Border, 1, draw.Uniform(8))
}
