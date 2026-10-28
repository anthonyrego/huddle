package text

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"github.com/go-text/typesetting/font/opentype/tables"
	"github.com/go-text/typesetting/fontscan"
)

// Font fallback. When a run's face lacks a character, the shaper asks for a
// system face that has it, chosen the way macOS's CoreText (and so Safari
// and Chrome) would: the cascade lists below are CTFontCopyDefaultCascade-
// ListForLanguages for Inter and JetBrains Mono on macOS 15, per CJK
// language, and emoji presentation follows UTS #51 with CoreText's choices
// (✅ and ⚠️ are emoji; ⚠ and ✔ are text).
//
// Candidates come from go-text's fontscan index of the system fonts, which
// is cached on disk in the data dir (the CLI renders too, and shares
// it). A face that is chosen is loaded once per process and gets a FontID
// from FirstFallback up; GlyphRuns carry that ID to the rasterizer.

// ---- system font index ----

// FontIndexInfo describes the system font index.
type FontIndexInfo struct {
	Path  string        // index file
	Fonts int           // faces indexed
	Load  time.Duration // time to load the index and check it against the font directories
	Built bool          // no index file existed: every font file was scanned
	Err   error
}

var sysIndex struct {
	once     sync.Once
	dir      string // set by SetFontIndexDir before first use
	fps      []fontscan.Footprint
	byFamily map[string][]int
	info     FontIndexInfo

	extrasOnce sync.Once
	extras     []extraFace // faces outside fontscan's directories
}

// SetFontIndexDir sets the directory the system font index is kept in.
// Call it before any text needs a fallback; the default is "fonts" under
// the data dir (config.Resolve).
func SetFontIndexDir(dir string) { sysIndex.dir = dir }

// PreloadFonts loads the system font index in the background, so the
// first line that needs a fallback doesn't wait for it (about 60 ms with a
// saved index). Call it at startup; calling it again does nothing.
func PreloadFonts() { go systemFonts() }

// FontIndex loads the system font index if needed and describes it.
func FontIndex() FontIndexInfo {
	systemFonts()
	return sysIndex.info
}

// fontscanLog routes fontscan's chatter to debug logging.
type fontscanLog struct{}

func (fontscanLog) Printf(format string, args ...any) {
	slog.Debug("fontscan: " + fmt.Sprintf(format, args...))
}

const indexPattern = "font_index_v*.cache" // fontscan's file name

func systemFonts() {
	sysIndex.once.Do(func() {
		dir := sysIndex.dir
		if dir == "" {
			p, err := config.Resolve()
			if err != nil {
				sysIndex.info.Err = err
				return
			}
			dir = filepath.Join(p.Data, "fonts")
		}
		start := time.Now()
		fps, built, path, err := loadIndex(dir)
		info := FontIndexInfo{Path: path, Fonts: len(fps), Load: time.Since(start), Built: built, Err: err}
		sysIndex.fps = fps
		sysIndex.byFamily = map[string][]int{}
		for i, fp := range fps {
			sysIndex.byFamily[fp.Family] = append(sysIndex.byFamily[fp.Family], i)
		}
		sysIndex.info = info
		log := slog.Debug
		if info.Built || err != nil {
			log = slog.Info // a full scan is slow enough to note
		}
		log("text: system font index", "fonts", info.Fonts, "load", info.Load.Round(time.Millisecond), "built", info.Built, "path", info.Path, "err", err)
		// The index is about 1 MB live, but fontscan churns ~50 MB loading and
		// rewriting it (gzip, a full re-serialization). Hand that back now
		// rather than carrying it in RSS until the scavenger gets to it.
		debug.FreeOSMemory()
	})
}

// loadIndex runs fontscan with its index file in dir. fontscan rewrites the
// file in place on every load, and the app and the CLI may start together,
// so each process works on a private copy and publishes it with a rename:
// a reader never sees a half-written index.
func loadIndex(dir string) (fps []fontscan.Footprint, built bool, path string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, "", err
	}
	work, err := os.MkdirTemp(dir, "scan-")
	if err != nil {
		return nil, false, "", err
	}
	defer os.RemoveAll(work)
	existing, _ := filepath.Glob(filepath.Join(dir, indexPattern))
	for _, f := range existing {
		if data, err := os.ReadFile(f); err == nil {
			os.WriteFile(filepath.Join(work, filepath.Base(f)), data, 0o644)
		}
	}
	fps, err = fontscan.SystemFonts(fontscanLog{}, work)
	written, _ := filepath.Glob(filepath.Join(work, indexPattern))
	for _, f := range written {
		path = filepath.Join(dir, filepath.Base(f))
		if rerr := os.Rename(f, path); rerr != nil {
			slog.Warn("text: saving the font index", "err", rerr)
		}
	}
	return fps, len(existing) == 0, path, err
}

// extraFace is a face outside fontscan's directories. macOS keeps PingFang,
// its default CJK face, in a font asset directory fontscan doesn't scan.
type extraFace struct {
	loc    font.FontID
	family string
	aspect font.Aspect

	once  sync.Once
	runes fontscan.RuneSet
}

// extraFontGlobs are asset directories CoreText uses that fontscan doesn't.
var extraFontGlobs = []string{
	"/System/Library/AssetsV2/com_apple_MobileAsset_Font*/*/AssetData/*.tt[cf]",
	"/System/Library/AssetsV2/com_apple_MobileAsset_Font*/*/AssetData/*.otf",
}

// extraFaces lists the faces in extraFontGlobs: names only, no coverage.
func extraFaces() []extraFace {
	sysIndex.extrasOnce.Do(func() {
		var buf []byte
		for _, g := range extraFontGlobs {
			files, _ := filepath.Glob(g)
			for _, path := range files {
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				lds, _ := ot.NewLoaders(f)
				for i, ld := range lds {
					var d font.Description
					d, buf = font.Describe(ld, buf)
					sysIndex.extras = append(sysIndex.extras, extraFace{
						loc:    font.FontID{File: path, Index: uint16(i)},
						family: font.NormalizeFamily(d.Family),
						aspect: d.Aspect,
					})
				}
				f.Close()
			}
		}
	})
	return sysIndex.extras
}

// coverage reads the face's cmap on first use.
func (e *extraFace) coverage() *fontscan.RuneSet {
	e.once.Do(func() {
		f, err := os.Open(e.loc.File)
		if err != nil {
			return
		}
		defer f.Close()
		lds, err := ot.NewLoaders(f)
		if err != nil || int(e.loc.Index) >= len(lds) {
			return
		}
		raw, err := lds[e.loc.Index].RawTable(ot.MustNewTag("cmap"))
		if err != nil {
			return
		}
		tb, _, err := tables.ParseCmap(raw)
		if err != nil {
			return
		}
		cmap, _, err := font.ProcessCmap(tb, tables.FPNone)
		if err != nil {
			return
		}
		it := cmap.Iter()
		for it.Next() {
			r, _ := it.Char()
			e.runes.Add(r)
		}
	})
	return &e.runes
}

// ---- fallback faces ----

// fallbackFace is a system face the shaper fell back to.
type fallbackFace struct {
	id     draw.FontID
	loc    font.FontID
	family string // normalized
	font   *font.Font
	// sbix holds color bitmaps left on disk; nil for outline faces.
	sbix *sbixTable
	// appleEmoji: draw with CoreText's emoji sizing (emojiSize, emojiDrop).
	appleEmoji bool
}

// lastFallback is the highest fallback FontID; TestColorFont sits above.
const lastFallback = TestColorFont - 1

var registry struct {
	mu      sync.RWMutex
	loading sync.Mutex      // one face loads at a time, outside mu
	faces   []*fallbackFace // by id - FirstFallback
	byLoc   map[font.FontID]*fallbackFace
	bad     map[font.FontID]bool
}

// fallbackByID returns the fallback face with the given ID, or nil.
func fallbackByID(id draw.FontID) *fallbackFace {
	if id < FirstFallback || id > lastFallback {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if i := int(id - FirstFallback); i < len(registry.faces) {
		return registry.faces[i]
	}
	return nil
}

// IsColorFont reports whether a font's glyphs are color bitmaps, drawn from
// the RGBA atlas at whole-pixel positions. It inlines to one comparison for
// the embedded faces: the batch asks for every glyph run.
func IsColorFont(id draw.FontID) bool { return id >= FirstFallback && isColorFallback(id) }

//go:noinline // keeps IsColorFont within the inlining budget
func isColorFallback(id draw.FontID) bool {
	if id == TestColorFont {
		return true
	}
	f := fallbackByID(id)
	return f != nil && f.sbix != nil
}

// FontName names a font for logs and reports: the embedded face, or the
// family and file of a fallback.
func FontName(id draw.FontID) string {
	if int(id) < len(embeddedFiles) {
		return strings.TrimSuffix(filepath.Base(embeddedFiles[id]), ".ttf")
	}
	if id == TestColorFont {
		return "synthetic color"
	}
	if f := fallbackByID(id); f != nil {
		return fmt.Sprintf("%s (%s#%d)", f.family, filepath.Base(f.loc.File), f.loc.Index)
	}
	return fmt.Sprintf("font %d", id)
}

// registerFace loads a system face and gives it a FontID, once per
// location.
func registerFace(loc font.FontID, family string) (*fallbackFace, error) {
	lookup := func() (*fallbackFace, error) {
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		if registry.bad[loc] {
			return nil, fmt.Errorf("%s#%d failed to load", loc.File, loc.Index)
		}
		return registry.byLoc[loc], nil
	}
	if f, err := lookup(); f != nil || err != nil {
		return f, err
	}

	// Load outside the registry lock: the render thread reads the registry
	// (IsColorFont) and must not wait on a font being parsed.
	registry.loading.Lock()
	defer registry.loading.Unlock()
	if f, err := lookup(); f != nil || err != nil {
		return f, err
	}
	start := time.Now()
	ft, sb, err := loadFace(loc)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.byLoc == nil {
		registry.byLoc = map[font.FontID]*fallbackFace{}
		registry.bad = map[font.FontID]bool{}
	}
	if err == nil && len(registry.faces) > int(lastFallback-FirstFallback) {
		err = fmt.Errorf("out of fallback font IDs (%d faces loaded)", len(registry.faces))
	}
	if err != nil {
		registry.bad[loc] = true
		slog.Warn("text: fallback face failed to load", "file", loc.File, "index", loc.Index, "err", err)
		return nil, err
	}
	f := &fallbackFace{
		id:         FirstFallback + draw.FontID(len(registry.faces)),
		loc:        loc,
		family:     family,
		font:       ft,
		sbix:       sb,
		appleEmoji: sb != nil && family == "applecoloremoji",
	}
	registry.faces = append(registry.faces, f)
	registry.byLoc[loc] = f
	slog.Debug("text: fallback face", "id", f.id, "family", family, "file", loc.File, "index", loc.Index, "sbix", sb != nil, "load", time.Since(start).Round(time.Microsecond))
	return f, nil
}

// ---- choosing a face ----

// class is what a primary face asks of its fallbacks.
type class struct {
	mono   bool
	weight font.Weight
	italic bool
}

func classOf(primary draw.FontID) class {
	switch primary {
	case SansSemibold:
		return class{weight: font.WeightSemibold}
	case SansItalic:
		return class{weight: font.WeightNormal, italic: true}
	case MonoRegular:
		return class{mono: true, weight: font.WeightNormal}
	case MonoBold:
		return class{mono: true, weight: font.WeightBold}
	}
	return class{weight: font.WeightNormal}
}

// cjkLang picks the CJK cascade: which of Chinese (simplified or
// traditional), Japanese or Korean faces comes first for Han characters.
type cjkLang uint8

const (
	cjkSC cjkLang = iota
	cjkTC
	cjkJA
	cjkKO
)

// defaultCJK follows the user's locale when the text itself doesn't say
// (kana means Japanese, Hangul Korean). CoreText with an English locale
// uses PingFang SC.
var defaultCJK = sync.OnceValue(func() cjkLang {
	for _, v := range []string{os.Getenv("LC_ALL"), os.Getenv("LC_CTYPE"), os.Getenv("LANG")} {
		v = strings.ToLower(v)
		switch {
		case v == "":
			continue
		case strings.HasPrefix(v, "ja"):
			return cjkJA
		case strings.HasPrefix(v, "ko"):
			return cjkKO
		case strings.HasPrefix(v, "zh_tw"), strings.HasPrefix(v, "zh_hk"), strings.HasPrefix(v, "zh-hant"), strings.HasPrefix(v, "zh_mo"):
			return cjkTC
		}
		return cjkSC
	}
	return cjkSC
})

func isKana(r rune) bool {
	return r >= 0x3040 && r <= 0x30FF || r >= 0x31F0 && r <= 0x31FF || r >= 0xFF66 && r <= 0xFF9F
}

func isHangul(r rune) bool {
	return r >= 0xAC00 && r <= 0xD7AF || r >= 0x1100 && r <= 0x11FF || r >= 0x3130 && r <= 0x318F
}

// isCJK covers Han, kana, Hangul and CJK punctuation and forms.
func isCJK(r rune) bool {
	return r >= 0x3000 && r <= 0x9FFF || r >= 0xAC00 && r <= 0xD7AF || r >= 0x1100 && r <= 0x11FF ||
		r >= 0xF900 && r <= 0xFAFF || r >= 0xFF00 && r <= 0xFFEF || r >= 0x20000 && r <= 0x3FFFF
}

// cjkLangs sets, for each rune of text, the language its Han characters are
// read in: a run of CJK characters containing kana is Japanese, one with
// Hangul is Korean, and anything else follows the locale (defaultCJK), the
// way a browser picks a face for text with no lang attribute.
func cjkLangs(text []rune, out []cjkLang) {
	def := defaultCJK()
	for i := 0; i < len(text); {
		if !isCJK(text[i]) {
			out[i] = def
			i++
			continue
		}
		lang, j := def, i
		for ; j < len(text) && isCJK(text[j]); j++ {
			switch {
			case isKana(text[j]):
				lang = cjkJA
			case isHangul(text[j]) && lang != cjkJA:
				lang = cjkKO
			}
		}
		for k := i; k < j; k++ {
			out[k] = lang
		}
		i = j
	}
}

var cjkFamilies = [...][]string{
	cjkSC: {"pingfangsc", "pingfangtc", "pingfanghk", "hiraginosans", "hiraginosansgb", "applesdgothicneo"},
	cjkTC: {"pingfangtc", "pingfangsc", "pingfanghk", "hiraginosans", "hiraginosansgb", "applesdgothicneo"},
	cjkJA: {"hiraginosans", "hiraginosansgb", "pingfangsc", "pingfangtc", "pingfanghk", "applesdgothicneo"},
	cjkKO: {"applesdgothicneo", "pingfangsc", "pingfangtc", "pingfanghk", "hiraginosans", "hiraginosansgb"},
}

// The cascades, minus the CJK block (inserted by language). The macOS
// families come from CoreText; the rest are common Linux and Windows faces
// so the same code finds something there.
var (
	sansHead = []string{"helvetica", "lucidagrande", "applesymbols"}
	monoHead = []string{"menlo", "monaco", "applesymbols", "couriernew", "helvetica", "lucidagrande"}
	sansTail = []string{
		"geezapro", "notonastaliqurdu", "notosansmyanmar", "thonburi", "kohinoordevanagari", "kailasa",
		"kohinoorbangla", "kohinoorgujarati", "muktamahee", "notosanskannada", "khmersangammn", "laosangammn",
		"malayalamsangammn", "notosansoriya", "sinhalasangammn", "tamilsangammn", "kohinoortelugu",
		"notosansarmenian", "euphemiaucas", "menlo", "stixtwomath", "galvji", "kefa", "zapfdingbats",
		"segoeui", "segoeuisymbol", "dejavusans", "notosans", "notosanssymbols", "notosanssymbols2", "notosansmath",
		"notosanscjksc", "notosanscjkjp", "notosanscjkkr", "notosanscjktc",
	}
	monoTail = []string{
		"geezapro", "notonastaliqurdu", "notosansmyanmar", "ayuthaya", "kohinoordevanagari", "kailasa",
		"kohinoorbangla", "kohinoorgujarati", "muktamahee", "notosanskannada", "khmersangammn", "laosangammn",
		"malayalamsangammn", "notosansoriya", "sinhalasangammn", "tamilsangammn", "kohinoortelugu",
		"notosansarmenian", "euphemiaucas", "stixtwomath", "galvji", "kefa", "zapfdingbats",
		"consolas", "segoeuisymbol", "dejavusansmono", "notosansmono", "liberationmono", "dejavusans",
		"notosanssymbols", "notosanssymbols2", "notosansmath",
		"notosanscjksc", "notosanscjkjp", "notosanscjkkr", "notosanscjktc",
	}
	emojiFamilies = []string{"applecoloremoji"}
)

// dingbats: CoreText draws the Dingbats block (✔ ✘ ✖ ➜) from Zapf Dingbats
// under a proportional face, ahead of its cascade.
const dingbatsFirst, dingbatsLast = 0x2700, 0x27BF

func cascade(c class, lang cjkLang, r rune) []string {
	head, tail := sansHead, sansTail
	if c.mono {
		head, tail = monoHead, monoTail
	}
	out := make([]string, 0, len(head)+len(tail)+8)
	if !c.mono && r >= dingbatsFirst && r <= dingbatsLast {
		out = append(out, "zapfdingbats")
	}
	out = append(out, head...)
	out = append(out, cjkFamilies[lang]...)
	return append(out, tail...)
}

type resolveKey struct {
	primary draw.FontID
	lang    cjkLang
	emoji   bool
	r       rune
}

var resolved struct {
	mu sync.Mutex
	m  map[resolveKey]draw.FontID
}

// resolveFallback returns the face that should draw r (the first rune of a
// cluster) as text in primary's place: a fallback face's ID, or primary when
// no system face has r (logged once per rune). The answer is cached for the
// process; the first call for a rune may load the system font index and a
// face from disk.
func resolveFallback(primary draw.FontID, r rune, lang cjkLang) draw.FontID {
	return resolve(resolveKey{primary: primary, lang: lang, r: r})
}

// resolveEmoji returns the emoji font's ID if it has r, else primary.
func resolveEmoji(primary draw.FontID, r rune) draw.FontID {
	return resolve(resolveKey{primary: primary, emoji: true, r: r})
}

func resolve(key resolveKey) draw.FontID {
	resolved.mu.Lock()
	id, ok := resolved.m[key]
	resolved.mu.Unlock()
	if ok {
		return id
	}
	id = key.primary
	if f := findFace(classOf(key.primary), key.r, key.lang, key.emoji); f != nil {
		id = f.id
	} else if !key.emoji {
		logMissing(key.r)
	}
	resolved.mu.Lock()
	if resolved.m == nil {
		resolved.m = map[resolveKey]draw.FontID{}
	}
	resolved.m[key] = id
	resolved.mu.Unlock()
	return id
}

func findFace(c class, r rune, lang cjkLang, emoji bool) *fallbackFace {
	systemFonts()
	if emoji {
		return firstFamily(emojiFamilies, c, r)
	}
	if f := firstFamily(cascade(c, lang, r), c, r); f != nil {
		return f
	}
	if f := anyFace(c, r); f != nil {
		return f
	}
	// CoreText's cascade ends with Apple Color Emoji, too.
	return firstFamily(emojiFamilies, c, r)
}

// firstFamily returns the first family in families with a face covering r,
// loading it. Within a family the face closest to c's weight and style is
// used.
func firstFamily(families []string, c class, r rune) *fallbackFace {
	for _, fam := range families {
		idx := sysIndex.byFamily[fam]
		if len(idx) > 0 {
			best := -1
			for _, i := range idx {
				if best < 0 || aspectScore(sysIndex.fps[i].Aspect, c) < aspectScore(sysIndex.fps[best].Aspect, c) {
					best = i
				}
			}
			fp := &sysIndex.fps[best]
			if !fp.Runes.Contains(r) {
				continue
			}
			if f, err := registerFace(fp.Location, fam); err == nil {
				return f
			}
			continue
		}
		// Not indexed: maybe a macOS font asset.
		var best *extraFace
		extras := extraFaces()
		for i := range extras {
			e := &extras[i]
			if e.family == fam && (best == nil || aspectScore(e.aspect, c) < aspectScore(best.aspect, c)) {
				best = e
			}
		}
		if best != nil && best.coverage().Contains(r) {
			if f, err := registerFace(best.loc, fam); err == nil {
				return f
			}
		}
	}
	return nil
}

// anyFace searches the whole index for a face covering r, preferring the
// class's spacing and aspect. Hidden system faces (a leading ".") and the
// LastResort placeholder font are skipped.
func anyFace(c class, r rune) *fallbackFace {
	best := -1
	var bestScore float32
	for i := range sysIndex.fps {
		fp := &sysIndex.fps[i]
		if fp.Family == "lastresort" || strings.HasPrefix(fp.Family, ".") || !fp.Runes.Contains(r) {
			continue
		}
		s := aspectScore(fp.Aspect, c)
		if isMonoFamily(fp.Family) != c.mono {
			s += 5000
		}
		if best < 0 || s < bestScore {
			best, bestScore = i, s
		}
	}
	if best < 0 {
		return nil
	}
	f, err := registerFace(sysIndex.fps[best].Location, sysIndex.fps[best].Family)
	if err != nil {
		return nil
	}
	return f
}

func isMonoFamily(fam string) bool {
	return strings.Contains(fam, "mono") || fam == "menlo" || fam == "monaco" || strings.HasPrefix(fam, "courier") || fam == "consolas"
}

// aspectScore ranks a face for class c: lower is closer. Style dominates,
// then weight distance (a lighter face wins a tie, like CSS for weights up
// to 500), then width.
func aspectScore(a font.Aspect, c class) float32 {
	var s float32
	if (a.Style == font.StyleItalic) != c.italic {
		s += 1000
	}
	d := float32(a.Weight - c.weight)
	if d < 0 {
		s += -d
	} else {
		s += d + 1
	}
	if a.Stretch != font.StretchNormal && a.Stretch != 0 {
		s += 500
	}
	return s
}

// missing records runes no face could draw, so each is logged once.
var missing sync.Map

func logMissing(r rune) {
	if _, seen := missing.LoadOrStore(r, true); !seen {
		slog.Warn("text: no font has a glyph", "rune", fmt.Sprintf("U+%04X", r), "char", string(r))
	}
}

// FallbackStats counts fallback work since the process started.
type FallbackStats struct {
	Faces     int   // system faces loaded
	SbixBytes int64 // color bitmap data read from disk (strike headers and PNGs)
}

// Fallbacks reports FallbackStats.
func Fallbacks() FallbackStats {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return FallbackStats{Faces: len(registry.faces), SbixBytes: sbixBytes.Load()}
}
