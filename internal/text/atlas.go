package text

// Atlas packs bitmaps into square pages on the CPU. internal/gfx mirrors each
// page in a GPU texture and uploads only the dirty spans (Page.Dirty).
//
// Packing is by shelves: a page is cut into horizontal strips whose height is
// the glyph height rounded up to a bucket, filled left to right. Glyphs of
// similar height share a shelf, so waste stays small for text.
//
// Eviction is least recently used, by page: when every page is full, the page
// whose glyphs were drawn longest ago is cleared and its entries dropped. A
// page used in the current frame is never evicted (its glyphs are already in
// this frame's instances); if all are, the atlas grows past MaxPages. Pages
// are never freed (glyph entries refer to them by index): later frames
// evict among them instead.
type Atlas struct {
	PageSize int
	BPP      int // bytes per pixel: 1 for coverage, 4 for RGBA
	MaxPages int
	Pages    []*Page

	// onEvict is called with the keys of a page before it is cleared.
	onEvict func(keys []uint64)
}

// Page is one atlas texture's CPU copy.
type Page struct {
	Pix      []byte // PageSize*PageSize*BPP, rows tightly packed
	shelves  []shelf
	nextY    int // top of the unused area below the last shelf
	lastUsed uint64
	keys     []uint64
}

type shelf struct {
	y, h, x  int // x: next free column
	dirtyX0  int
	dirtyX1  int // dirty span [dirtyX0, dirtyX1); empty when equal
	hasDirty bool
}

// Region is a rectangle of a page, in pixels.
type Region struct{ X, Y, W, H int }

// gutter is the empty border kept to the right of and below each glyph, so a
// linear sampler (scaled or transformed text) never bleeds in a neighbor.
const gutter = 1

// NewAtlas returns an empty atlas. Pages are created on demand.
func NewAtlas(pageSize, bpp, maxPages int) *Atlas {
	return &Atlas{PageSize: pageSize, BPP: bpp, MaxPages: max(maxPages, 1)}
}

func shelfBucket(h int) int { return (h + 3) &^ 3 }

// alloc reserves w×h pixels (plus gutter) and returns where. frame is the
// current frame; it marks the page used. ok is false if the bitmap can never
// fit (larger than a page).
func (a *Atlas) alloc(w, h int, frame uint64, key uint64) (page, x, y int, evicted bool, ok bool) {
	gw, gh := w+gutter, h+gutter
	if gw > a.PageSize || gh > a.PageSize {
		return 0, 0, 0, false, false
	}
	for i, p := range a.Pages {
		if x, y, ok := a.place(p, gw, gh); ok {
			p.lastUsed = frame
			p.keys = append(p.keys, key)
			return i, x, y, false, true
		}
	}
	idx := -1
	if len(a.Pages) < a.MaxPages {
		a.Pages = append(a.Pages, &Page{Pix: make([]byte, a.PageSize*a.PageSize*a.BPP)})
		idx = len(a.Pages) - 1
	} else {
		// Evict the least recently used page not drawn from this frame.
		oldest := frame
		for i, p := range a.Pages {
			if p.lastUsed < oldest {
				oldest, idx = p.lastUsed, i
			}
		}
		if idx >= 0 {
			a.clearPage(a.Pages[idx])
			evicted = true
		} else {
			a.Pages = append(a.Pages, &Page{Pix: make([]byte, a.PageSize*a.PageSize*a.BPP)})
			idx = len(a.Pages) - 1
		}
	}
	p := a.Pages[idx]
	x, y, _ = a.place(p, gw, gh)
	p.lastUsed = frame
	p.keys = append(p.keys, key)
	return idx, x, y, evicted, true
}

// place finds room for a gw×gh cell on p's shelves, opening a shelf if needed.
func (a *Atlas) place(p *Page, gw, gh int) (x, y int, ok bool) {
	best := -1
	for i := range p.shelves {
		s := &p.shelves[i]
		// Reuse a shelf that fits without wasting more than half its height.
		if s.h >= gh && s.h <= gh+gh/2+4 && s.x+gw <= a.PageSize {
			if best < 0 || s.h < p.shelves[best].h {
				best = i
			}
		}
	}
	if best < 0 {
		h := shelfBucket(gh)
		if p.nextY+h > a.PageSize {
			h = gh // last rows of the page: take an exact fit
			if p.nextY+h > a.PageSize {
				return 0, 0, false
			}
		}
		// A new shelf uploads its full width once, so every texel of a used
		// shelf is defined: a filtered sample at a glyph's top edge reads
		// this shelf's zeros, never stale data from an evicted page or an
		// uninitialized texture.
		p.shelves = append(p.shelves, shelf{y: p.nextY, h: h, dirtyX0: 0, dirtyX1: a.PageSize, hasDirty: true})
		p.nextY += h
		best = len(p.shelves) - 1
	}
	s := &p.shelves[best]
	x, y = s.x, s.y
	s.x += gw
	if !s.hasDirty {
		s.dirtyX0, s.dirtyX1, s.hasDirty = x, x+gw, true
	} else {
		s.dirtyX0, s.dirtyX1 = min(s.dirtyX0, x), max(s.dirtyX1, x+gw)
	}
	return x, y, true
}

// clearPage empties a page, dropping its entries. Pixels are zeroed so the
// gutters around new glyphs are transparent; the whole cell (glyph plus
// gutter) is marked dirty when reused, so stale GPU texels are overwritten.
func (a *Atlas) clearPage(p *Page) {
	if a.onEvict != nil {
		a.onEvict(p.keys)
	}
	clear(p.Pix)
	p.shelves = p.shelves[:0]
	p.nextY = 0
	p.keys = p.keys[:0]
}

// reset empties every page.
func (a *Atlas) reset() {
	for _, p := range a.Pages {
		clear(p.Pix)
		p.shelves = p.shelves[:0]
		p.nextY = 0
		p.keys = p.keys[:0]
		p.lastUsed = 0
	}
}

// write copies a bitmap's rows into the page at (x, y).
func (a *Atlas) write(p *Page, x, y int, b *Bitmap) {
	row := b.W * a.BPP
	stride := a.PageSize * a.BPP
	for r := range b.H {
		copy(p.Pix[(y+r)*stride+x*a.BPP:], b.Pix[r*row:(r+1)*row])
	}
}

// Dirty calls fn for each region of the page changed since the last
// ClearDirty. Regions are spans of one shelf, so each is a single upload.
func (p *Page) Dirty(fn func(r Region)) {
	for i := range p.shelves {
		s := &p.shelves[i]
		if s.hasDirty {
			fn(Region{X: s.dirtyX0, Y: s.y, W: s.dirtyX1 - s.dirtyX0, H: s.h})
		}
	}
}

// ClearDirty marks the page clean. Call it once the regions Dirty reported
// are uploaded; after a failed upload they stay dirty and go next frame.
func (p *Page) ClearDirty() {
	for i := range p.shelves {
		p.shelves[i].hasDirty = false
	}
}

// Used reports how many rows of the page are taken by shelves.
func (p *Page) Used() int { return p.nextY }
