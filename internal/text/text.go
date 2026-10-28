// Package text shapes and wraps styled text. It is pure Go (go-text/typesetting)
// and never touches the GPU: it turns Runs into positioned draw.GlyphRuns, and
// internal/gfx rasterizes those glyphs into its atlas.
//
// Sizes and positions are logical units, the same as package draw.
//
// Copied from an earlier project of the author's.
package text

import "github.com/anthonyrego/huddle/internal/draw"

// The embedded faces. System faces the shaper falls back to get IDs from
// FirstFallback up (fallback.go).
const (
	SansRegular  draw.FontID = iota // Inter Regular
	SansSemibold                    // Inter SemiBold
	SansItalic                      // Inter Italic
	MonoRegular                     // JetBrains Mono Regular
	MonoBold                        // JetBrains Mono Bold

	FirstFallback
)

// IsMono reports whether a font is one of the monospace faces. Monospace text
// is shaped with ligatures and contextual alternates off.
func IsMono(f draw.FontID) bool { return f == MonoRegular || f == MonoBold }

// Style is how a Run looks.
type Style struct {
	Font  draw.FontID
	Size  float32 // logical px
	Color draw.Color
	// LineHeight is a multiple of Size; 0 uses the font's own line spacing.
	LineHeight float32
}

// Run is a span of text in one style. A paragraph is a []Run; "\n" in Text
// forces a line break.
type Run struct {
	Text  string
	Style Style
}

// Layout is shaped, wrapped text. Coordinates are relative to the layout's
// top-left corner.
type Layout struct {
	Width, Height float32
	Lines         []Line
}

// Line is one visual line.
type Line struct {
	Top      float32 // y of the line box's top edge
	Baseline float32 // y of the baseline
	Height   float32 // line box height
	Width    float32 // advance width, trailing whitespace trimmed
	Runs     []draw.GlyphRun
}

// Measurer lays out text. Code that only needs sizes (diagram node sizing,
// layout) depends on this interface so tests can supply a fake.
type Measurer interface {
	// Layout shapes runs and wraps them at maxWidth. maxWidth <= 0 means no
	// wrapping: only "\n" breaks lines.
	Layout(runs []Run, maxWidth float32) Layout
}
