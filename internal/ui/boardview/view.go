// Package boardview draws the board: boxes laid out left to right in the
// order of the work, the satellites under them, frames around groups, and
// the arrows. When the board changes, boxes glide to their new places and
// new ones fade in; the camera keeps the whole board in view until the user
// pans or zooms. Pure: the window and offscreen renders use the same view.
//
// Copied from an earlier project of the author's.
package boardview

import (
	"math"
	"slices"
	"time"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
	"github.com/anthonyrego/huddle/internal/ui"
)

const (
	moveDur  = 380 * time.Millisecond // boxes glide to new places
	bornDur  = 320 * time.Millisecond // new boxes fade in
	camDur   = 420 * time.Millisecond // the camera follows the board
	pointIn  = 140 * time.Millisecond
	pointOut = 500 * time.Millisecond

	maxFit     = 1.25 // the camera never fits closer than this
	minScale   = 0.2
	maxScale   = 3
	dragSlop   = 3
	wheelZoom  = 300 // wheel units per factor e
	zoomSettle = 160 * time.Millisecond
	zoomSteps  = 6

	groupPad    = 14
	groupLabelH = 22
	gridStep    = 28

	haloW     = 4.5  // the halo under arrows and stems over the desktop
	haloAlpha = 0xb0 // its alpha, of Background
)

// spacing is the board's layout spacing: wider than the tree layout it came
// from, so group frames fit between cards.
var spacing = layout.Style{RankGap: 72, NodeGap: 44, LaneGap: 16, BandGap: 64, Margin: 48}

// View is the board widget.
type View struct {
	// OnClick is called with the id of a box (or satellite or group) the
	// user clicked.
	OnClick func(id string)
	// Overlay draws the board over the desktop: no grid and no hint, and a
	// halo under every line so it reads over anything. Cards and frames stay
	// opaque: whatever showed through them would read as part of the board.
	Overlay bool

	doc        *board.Doc
	dirty      bool
	pendingNow time.Time // when SetDoc was called: glides start then
	cards      map[string]*card
	out        *layout.Output

	from   map[string]draw.Rect // where each box starts its glide
	moveAt time.Time
	born   map[string]time.Time

	width float32
	vh    float32
	size  ui.Size

	scale  float32
	off    draw.Point
	fitted bool
	cam    camMove
	zoomAt time.Time

	points map[string]pointing
	hover  string
	press  *press

	swapped bool // Reset gave it another board: snap the camera, fade the board in
}

type camMove struct {
	at                 time.Time
	fromScale, toScale float32
	fromOff, toOff     draw.Point
}

type pointing struct{ from, until time.Time }

type press struct {
	at      draw.Point
	off     draw.Point
	id      string
	dragged bool
}

// New returns an empty board view that fits the board as it grows.
func New() *View {
	return &View{doc: board.New(), cards: map[string]*card{}, born: map[string]time.Time{}, points: map[string]pointing{}, fitted: true, dirty: true}
}

// SetDoc shows a new state of the board. Boxes that move glide from where
// they are now.
func (v *View) SetDoc(d *board.Doc, now time.Time) {
	v.doc = d
	v.dirty = true
	v.pendingNow = now
}

// Reset shows another board in place of this one. Nothing glides from the
// old board: the camera snaps to fit the new one and its boxes fade in.
func (v *View) Reset(d *board.Doc, now time.Time) {
	v.doc = d
	v.cards, v.out, v.from = map[string]*card{}, nil, nil
	v.born, v.points = map[string]time.Time{}, map[string]pointing{}
	v.hover, v.press = "", nil
	v.fitted, v.cam, v.swapped = true, camMove{}, true
	v.dirty, v.pendingNow = true, now
}

// Doc is the board shown.
func (v *View) Doc() *board.Doc { return v.doc }

// Point highlights boxes or groups for d, starting at now.
func (v *View) Point(ids []string, d time.Duration, now time.Time) {
	for _, id := range ids {
		v.points[id] = pointing{from: now, until: now.Add(d)}
	}
}

// Layout is the current layout (after Measure).
func (v *View) Layout() *layout.Output { return v.out }

// SetViewport implements ui.ViewportFitter: the board fills its pane.
func (v *View) SetViewport(h float32) { v.vh = h }

// Fit puts the camera back on the whole board, gliding there.
func (v *View) Fit(now time.Time) {
	v.fitted = true
	v.follow(now)
}

// SnapFit puts the camera on the whole board at once, at whatever size the
// next Measure brings: for when the board moves to another window, where a
// glide from the old camera would mean nothing.
func (v *View) SnapFit() {
	v.fitted, v.cam, v.scale = true, camMove{}, 0
}

func (v *View) Measure(c *ui.Ctx, width float32) ui.Size {
	resized := width != v.width || v.vh != v.size.H
	v.width, v.size = width, ui.Size{W: width, H: v.vh}
	if v.dirty {
		v.relayout(c)
	}
	if v.scale == 0 {
		v.scale, v.off = v.fitTarget()
	} else if resized && v.fitted {
		v.scale, v.off = v.fitTarget()
		v.cam = camMove{}
	}
	return v.size
}

// relayout measures the cards that changed and lays the board out again,
// starting from the previous layout so as little as possible moves.
func (v *View) relayout(c *ui.Ctx) {
	v.dirty = false
	now := v.pendingNow
	if now.IsZero() {
		now = c.Now
	}
	nodes := v.doc.Nodes()
	cards := make(map[string]*card, len(nodes))
	var in layout.Input
	hasIn := map[string]bool{}
	for _, l := range v.doc.Links() {
		hasIn[l.To] = true
	}
	for _, n := range nodes {
		if n.On != "" {
			continue
		}
		sats := v.doc.Satellites(n.ID)
		cd := v.cards[n.ID]
		if cd == nil || cd.key != cardKey(n, sats) {
			cd = measureCard(c.Text, n, sats)
		}
		cards[n.ID] = cd
		in.Nodes = append(in.Nodes, layout.Node{ID: n.ID, W: cd.w, H: cd.totalH, In: cd.h / 2, Ports: []float32{cd.h / 2}})
		if !hasIn[n.ID] {
			in.Roots = append(in.Roots, n.ID)
		}
	}
	for _, l := range v.doc.Links() {
		in.Edges = append(in.Edges, layout.Edge{From: l.From, Port: 0, To: l.To})
	}
	in.Prev = v.out
	in.Style = spacing
	out := layout.Layout(in)

	// Glide from wherever each box is drawn right now.
	from := map[string]draw.Rect{}
	for id := range out.Boxes {
		if v.out != nil {
			if _, ok := v.out.Boxes[id]; ok {
				from[id] = v.boxAt(id, now)
				continue
			}
		}
		if v.out != nil || v.swapped { // the first layout shows everything at once; another board fades in
			v.born[id] = now
		}
	}
	for id := range v.born {
		if _, ok := out.Boxes[id]; !ok {
			delete(v.born, id)
		}
	}
	v.cards, v.out, v.from, v.moveAt = cards, out, from, now
	switch {
	case v.swapped:
		v.swapped = false
		v.scale, v.off = v.fitTarget()
	case v.fitted:
		v.follow(now)
	}
}

// boxAt is where a box is drawn at now: its card only (not the
// satellites), partway along its glide.
func (v *View) boxAt(id string, now time.Time) draw.Rect {
	to := v.out.Boxes[id]
	cd := v.cards[id]
	if cd != nil {
		to.H = cd.h
	}
	f, ok := v.from[id]
	if !ok {
		return to
	}
	t := ease(progress(now, v.moveAt, moveDur))
	return draw.Rect{X: lerp(f.X, to.X, t), Y: lerp(f.Y, to.Y, t), W: lerp(f.W, to.W, t), H: lerp(f.H, to.H, t)}
}

// fitTarget is the camera that shows the whole board, centered.
func (v *View) fitTarget() (float32, draw.Point) {
	if v.out == nil || v.out.W <= 0 || v.size.W <= 0 || v.size.H <= 0 {
		return 1, draw.Point{}
	}
	s := min(maxFit, v.size.W/v.out.W, v.size.H/v.out.H)
	return s, draw.Point{X: (v.size.W - v.out.W*s) / 2, Y: (v.size.H - v.out.H*s) / 2}
}

// follow glides the camera to the fit.
func (v *View) follow(now time.Time) {
	s, off := v.fitTarget()
	cs, co := v.camera(now)
	v.cam = camMove{at: now, fromScale: cs, toScale: s, fromOff: co, toOff: off}
	v.scale, v.off = s, off
}

// camera is the scale and offset drawn at now.
func (v *View) camera(now time.Time) (float32, draw.Point) {
	if v.cam.at.IsZero() {
		return v.scale, v.off
	}
	t := ease(progress(now, v.cam.at, camDur))
	if t >= 1 {
		return v.cam.toScale, v.cam.toOff
	}
	return lerp(v.cam.fromScale, v.cam.toScale, t), draw.Point{X: lerp(v.cam.fromOff.X, v.cam.toOff.X, t), Y: lerp(v.cam.fromOff.Y, v.cam.toOff.Y, t)}
}

// settle stops the camera glide where it is, before the user moves it.
func (v *View) settle(now time.Time) {
	v.scale, v.off = v.camera(now)
	v.cam = camMove{}
}

// Animating reports whether a glide, fade, highlight or camera move is
// still running at now.
func (v *View) Animating(now time.Time) bool {
	if now.Sub(v.moveAt) < moveDur || !v.cam.at.IsZero() && now.Sub(v.cam.at) < camDur || now.Sub(v.zoomAt) < zoomSettle {
		return true
	}
	for _, t := range v.born {
		if now.Sub(t) < bornDur {
			return true
		}
	}
	for _, p := range v.points {
		if now.Before(p.until.Add(pointOut)) {
			return true
		}
	}
	return false
}

func (v *View) toBoard(p draw.Point, now time.Time) draw.Point {
	s, off := v.camera(now)
	return draw.Point{X: (p.X - off.X) / s, Y: (p.Y - off.Y) / s}
}

// ZoomBy scales the board by k around at (widget coordinates).
func (v *View) ZoomBy(k float32, at draw.Point, now time.Time) {
	v.settle(now)
	if k <= 0 || v.scale == 0 {
		return
	}
	s := min(max(v.scale*k, minScale), maxScale)
	fx, fy := (at.X-v.off.X)/v.scale, (at.Y-v.off.Y)/v.scale
	v.scale = s
	v.off = draw.Point{X: at.X - fx*s, Y: at.Y - fy*s}
	v.fitted = false
	v.zoomAt = now
}

func (v *View) Event(c *ui.Ctx, ev ui.Event) ui.Result {
	switch ev.Kind {
	case ui.Pinch:
		v.ZoomBy(ev.Scale, ev.Pos, c.Now)
		return ui.Result{Handled: true, Redraw: true}
	case ui.Wheel:
		if ev.Mods&(ui.Cmd|ui.Ctrl) != 0 {
			v.ZoomBy(float32(math.Exp(float64(-ev.DY)/wheelZoom)), ev.Pos, c.Now)
			return ui.Result{Handled: true, Redraw: true}
		}
		v.settle(c.Now)
		v.off.X -= ev.DX
		v.off.Y -= ev.DY
		v.fitted = false
		return ui.Result{Handled: true, Redraw: true}
	case ui.PointerDown:
		if ev.Button != 1 {
			return ui.Result{}
		}
		if ev.Clicks == 2 {
			v.Fit(c.Now)
			v.press = nil
			return ui.Result{Handled: true, Redraw: true}
		}
		v.settle(c.Now)
		part, _ := v.Hit(c, ev.Pos)
		v.press = &press{at: ev.Pos, off: v.off, id: part.ID}
		return ui.Result{Handled: true}
	case ui.PointerMove:
		if p := v.press; p != nil {
			dx, dy := ev.Pos.X-p.at.X, ev.Pos.Y-p.at.Y
			if !p.dragged && dx*dx+dy*dy < dragSlop*dragSlop {
				return ui.Result{Handled: true}
			}
			p.dragged = true
			v.hover = ""
			v.off = draw.Point{X: p.off.X + dx, Y: p.off.Y + dy}
			v.fitted = false
			return ui.Result{Handled: true, Redraw: true}
		}
		part, _ := v.Hit(c, ev.Pos)
		if part.ID == v.hover {
			return ui.Result{}
		}
		v.hover = part.ID
		return ui.Result{Redraw: true}
	case ui.PointerUp:
		p := v.press
		v.press = nil
		if p != nil && !p.dragged && p.id != "" && v.OnClick != nil {
			if part, _ := v.Hit(c, ev.Pos); part.ID == p.id {
				v.OnClick(p.id)
			}
		}
		return ui.Result{Handled: true}
	case ui.KeyPress:
		if ev.Key == 'f' && ev.Mods == 0 {
			v.Fit(c.Now)
			return ui.Result{Handled: true, Redraw: true}
		}
	case ui.PointerLeave:
		v.press = nil
		if v.hover != "" {
			v.hover = ""
			return ui.Result{Redraw: true}
		}
	}
	return ui.Result{}
}

// Hover is the id under the pointer ("" for none).
func (v *View) Hover() string { return v.hover }

// SetHover sets the hovered id, for offscreen renders.
func (v *View) SetHover(id string) { v.hover = id }

func (v *View) Draw(c *ui.Ctx, l *draw.List, origin draw.Point, visible draw.Rect) {
	if v.dirty {
		v.relayout(c)
	}
	now := c.Now
	pal := c.Pal
	scale, off := v.camera(now)
	box := draw.Rect{X: origin.X, Y: origin.Y, W: v.size.W, H: v.size.H}
	var textSize func(float32) float32
	if v.Animating(now) {
		px := c.Scale * scale
		textSize = func(s float32) float32 { return snapTextSize(s, px) }
		c.RequestFrame()
	}

	l.PushClip(box)
	if !v.Overlay {
		v.drawGrid(l, c, box, scale, off)
	}
	if v.doc.Empty() {
		if v.Overlay {
			l.PopClip()
			return
		}
		hint := lay(c.Text, "Say “whiteboard, …” and it draws here.", style(text.SansRegular, 15), 0)
		putText(l, hint, box.X+(box.W-hint.Width)/2, box.Y+(box.H-hint.Height)/2, pal.TextFaint, nil)
		l.PopClip()
		return
	}
	l.PushTransform(draw.Transform{Scale: scale, Offset: draw.Point{X: origin.X + off.X, Y: origin.Y + off.Y}})

	var halo draw.Color
	if v.Overlay {
		halo = pal.Background.WithAlpha(haloAlpha)
	}

	// Frames first, behind everything.
	for _, g := range v.doc.Groups() {
		r, ok := v.groupRect(g.ID, now)
		if !ok {
			continue
		}
		l.Rect(r, pal.GroupFill, draw.Uniform(cardRadius+4))
		gl := lay(c.Text, g.Label, style(text.SansSemibold, 12.5), 0)
		putText(l, gl, r.X+groupPad, r.Y+(groupLabelH-gl.Height)/2+3, pal.TextMuted, textSize)
		v.drawPointing(l, c, g.ID, r, now)
		if v.hover == g.ID {
			l.BorderedRect(r, draw.Color{}, pal.Accent.WithAlpha(0x90), 1.5, draw.Uniform(cardRadius+4))
		}
	}

	gliding := now.Sub(v.moveAt) < moveDur
	for i, r := range v.out.Routes {
		pts := r.Points
		if gliding || r.Back || r.Cross {
			a, b := v.boxAt(r.Edge.From, now), v.boxAt(r.Edge.To, now)
			pts = curve(draw.Point{X: a.Right(), Y: a.Y + a.H/2}, draw.Point{X: b.X, Y: b.Y + b.H/2})
		}
		alpha := min(v.bornAlpha(r.Edge.From, now), v.bornAlpha(r.Edge.To, now))
		if halo.A > 0 {
			l.Path(draw.Path{Points: pts, Stroke: fade(halo, alpha), Width: haloW, ArrowEnd: draw.ArrowTriangle})
		}
		l.Path(draw.Path{Points: pts, Stroke: fade(pal.EdgeStroke, alpha), Width: 1.6, ArrowEnd: draw.ArrowTriangle})
		if lbl := v.linkLabel(i); lbl != "" {
			v.drawLinkLabel(l, c, lbl, pts, alpha, textSize)
		}
	}

	for _, n := range v.doc.Nodes() {
		cd := v.cards[n.ID]
		if cd == nil {
			continue
		}
		b := v.boxAt(n.ID, now)
		a := v.bornAlpha(n.ID, now)
		if a < 1 {
			// New boxes grow slightly as they fade in.
			k := 0.94 + 0.06*a
			b = draw.Rect{X: b.X + b.W*(1-k)/2, Y: b.Y + b.H*(1-k)/2, W: b.W * k, H: b.H * k}
		}
		v.drawPointing(l, c, n.ID, b, now)
		cd.draw(l, &pal, n.ID, b, look{alpha: a, hover: v.hover, textSize: textSize, halo: halo})
		for _, s := range cd.sats {
			v.drawPointing(l, c, s.id, s.rect(b), now)
		}
	}
	l.PopTransform()
	l.PopClip()
}

// linkLabel is the label of the route at index i (routes follow the
// links' order).
func (v *View) linkLabel(i int) string {
	links := v.doc.Links()
	if i < len(links) {
		return links[i].Label
	}
	return ""
}

func (v *View) drawLinkLabel(l *draw.List, c *ui.Ctx, s string, pts []draw.Point, a float32, textSize func(float32) float32) {
	if len(pts) == 0 {
		return
	}
	mid := pts[len(pts)/2]
	tl := lay(c.Text, s, style(text.SansRegular, 11.5), 0)
	r := draw.Rect{X: mid.X - tl.Width/2 - 6, Y: mid.Y - tl.Height/2 - 2, W: tl.Width + 12, H: tl.Height + 4}
	l.BorderedRect(r, fade(c.Pal.Surface, a), fade(c.Pal.Border, a), 1, draw.Uniform(r.H/2))
	putText(l, tl, r.X+6, r.Y+2, fade(c.Pal.TextMuted, a), textSize)
}

// drawPointing draws the glow of point_at around r.
func (v *View) drawPointing(l *draw.List, c *ui.Ctx, id string, r draw.Rect, now time.Time) {
	a := v.pointAlpha(id, now)
	if a <= 0 {
		return
	}
	// A slow pulse while it's held, so it reads as "this one".
	pulse := float32(0.75 + 0.25*math.Cos(float64(now.Sub(v.points[id].from))/float64(time.Millisecond)/180))
	l.Shadow(r.Inset(-3), c.Pal.Accent.WithAlpha(uint8(120*a*pulse)), draw.Uniform(cardRadius+3), 18)
	l.BorderedRect(r.Inset(-3), draw.Color{}, c.Pal.Accent.WithAlpha(uint8(255*a)), 2.5, draw.Uniform(cardRadius+3))
	c.RequestFrame()
}

func (v *View) pointAlpha(id string, now time.Time) float32 {
	p, ok := v.points[id]
	if !ok {
		return 0
	}
	switch d := now.Sub(p.from); {
	case d < 0:
		return 0
	case d < pointIn:
		return float32(d) / float32(pointIn)
	case now.Before(p.until):
		return 1
	case now.Before(p.until.Add(pointOut)):
		return 1 - float32(now.Sub(p.until))/float32(pointOut)
	}
	delete(v.points, id)
	return 0
}

func (v *View) bornAlpha(id string, now time.Time) float32 {
	t, ok := v.born[id]
	if !ok {
		return 1
	}
	return ease(progress(now, t, bornDur))
}

// groupRect is a frame around its members' cards and satellites.
func (v *View) groupRect(id string, now time.Time) (draw.Rect, bool) {
	var r draw.Rect
	found := false
	for _, n := range v.doc.Members(id) {
		cd := v.cards[n.ID]
		if cd == nil {
			continue
		}
		b := v.boxAt(n.ID, now)
		b.H = cd.totalH * b.H / cd.h
		if !found {
			r, found = b, true
			continue
		}
		r = union(r, b)
	}
	if !found {
		return draw.Rect{}, false
	}
	return draw.Rect{X: r.X - groupPad, Y: r.Y - groupPad - groupLabelH, W: r.W + 2*groupPad, H: r.H + 2*groupPad + groupLabelH}, true
}

// drawGrid draws the faint dot grid of a whiteboard, moving with the
// camera.
func (v *View) drawGrid(l *draw.List, c *ui.Ctx, box draw.Rect, scale float32, off draw.Point) {
	step := float32(gridStep) * scale
	for step < 14 {
		step *= 2
	}
	dot := max(1.5, 2*min(scale, 1))
	col := c.Pal.Border
	x0 := box.X + float32(math.Mod(float64(off.X), float64(step)))
	y0 := box.Y + float32(math.Mod(float64(off.Y), float64(step)))
	for y := y0; y < box.Bottom(); y += step {
		for x := x0; x < box.Right(); x += step {
			l.Rect(draw.Rect{X: x - dot/2, Y: y - dot/2, W: dot, H: dot}, col, draw.Uniform(dot/2))
		}
	}
}

// Hit returns the box, satellite or group under p (widget coordinates).
func (v *View) Hit(c *ui.Ctx, p draw.Point) (ui.Part, bool) {
	if v.out == nil {
		return ui.Part{}, false
	}
	now := c.Now
	q := v.toBoard(p, now)
	nodes := v.doc.Nodes()
	for _, n := range slices.Backward(nodes) {
		cd := v.cards[n.ID]
		if cd == nil {
			continue
		}
		b := v.boxAt(n.ID, now)
		for _, s := range cd.sats {
			if s.rect(b).Contains(q) {
				return ui.Part{Kind: ui.PartNode, ID: s.id}, true
			}
		}
		if b.Contains(q) {
			return ui.Part{Kind: ui.PartNode, ID: n.ID}, true
		}
	}
	for _, g := range slices.Backward(v.doc.Groups()) {
		if r, ok := v.groupRect(g.ID, now); ok && r.Contains(q) {
			return ui.Part{Kind: ui.PartNode, ID: g.ID}, true
		}
	}
	return ui.Part{}, false
}

// Locate returns where a box, satellite or group is, in widget coordinates.
func (v *View) Locate(c *ui.Ctx, part ui.Part) (draw.Rect, bool) {
	if v.out == nil {
		return draw.Rect{}, false
	}
	now := c.Now
	var r draw.Rect
	switch n := v.doc.Node(part.ID); {
	case n != nil && n.On != "":
		cd := v.cards[n.On]
		if cd == nil {
			return draw.Rect{}, false
		}
		i := slices.IndexFunc(cd.sats, func(s *sat) bool { return s.id == n.ID })
		if i < 0 {
			return draw.Rect{}, false
		}
		r = cd.sats[i].rect(v.boxAt(n.On, now))
	case n != nil:
		r = v.boxAt(n.ID, now)
	default:
		g, ok := v.groupRect(part.ID, now)
		if !ok {
			return draw.Rect{}, false
		}
		r = g
	}
	s, off := v.camera(now)
	return draw.Rect{X: r.X*s + off.X, Y: r.Y*s + off.Y, W: r.W * s, H: r.H * s}, true
}

// curve is an arrow out of a card's right side and into the next one's
// left, leaving and arriving horizontally.
func curve(p, q draw.Point) []draw.Point {
	d := max(float32(math.Abs(float64(q.X-p.X)))/2, 36)
	c1, c2 := draw.Point{X: p.X + d, Y: p.Y}, draw.Point{X: q.X - d, Y: q.Y}
	const steps = 20
	pts := make([]draw.Point, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float32(i) / steps
		u := 1 - t
		a, b, cc, dd := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		pts = append(pts, draw.Point{X: a*p.X + b*c1.X + cc*c2.X + dd*q.X, Y: a*p.Y + b*c1.Y + cc*c2.Y + dd*q.Y})
	}
	return pts
}

func union(a, b draw.Rect) draw.Rect {
	x, y := min(a.X, b.X), min(a.Y, b.Y)
	return draw.Rect{X: x, Y: y, W: max(a.Right(), b.Right()) - x, H: max(a.Bottom(), b.Bottom()) - y}
}

func progress(now, start time.Time, d time.Duration) float32 {
	if start.IsZero() {
		return 1
	}
	return min(max(float32(now.Sub(start))/float32(d), 0), 1)
}

// ease is ease-out cubic.
func ease(t float32) float32 { u := 1 - t; return 1 - u*u*u }

func lerp(a, b, t float32) float32 { return a + (b-a)*t }

func cos(a float32) float32 { return float32(math.Cos(float64(a))) }
func sin(a float32) float32 { return float32(math.Sin(float64(a))) }

// snapTextSize is the font size to draw a size-s label at while the board
// moves, at px pixels per board unit: the pixel size rounded to a step, so
// the glyph cache isn't filled with a size per frame.
//
// Copied from an earlier project of the author's.
func snapTextSize(s, px float32) float32 {
	if s <= 0 || px <= 0 {
		return s
	}
	target := float64(s * px)
	snapped := math.Exp2(math.Round(math.Log2(target)*zoomSteps) / zoomSteps)
	return float32(snapped) / px
}
