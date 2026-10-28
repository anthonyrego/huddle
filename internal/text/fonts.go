package text

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/go-text/typesetting/font"
)

//go:embed fonts/*.ttf
var fontFS embed.FS

var embeddedFiles = [...]string{
	SansRegular:  "fonts/Inter-Regular.ttf",
	SansSemibold: "fonts/Inter-SemiBold.ttf",
	SansItalic:   "fonts/Inter-Italic.ttf",
	MonoRegular:  "fonts/JetBrainsMono-Regular.ttf",
	MonoBold:     "fonts/JetBrainsMono-Bold.ttf",
}

var (
	fontsOnce sync.Once
	fonts     []*font.Font
	fontsErr  error
)

// embeddedFonts parses the embedded faces once. *font.Font is read-only and
// safe to share; each Shaper wraps them in its own *font.Face, which is not.
func embeddedFonts() ([]*font.Font, error) {
	fontsOnce.Do(func() {
		for _, name := range embeddedFiles {
			data, err := fontFS.ReadFile(name)
			if err != nil {
				fontsErr = err
				return
			}
			face, err := font.ParseTTF(bytes.NewReader(data))
			if err != nil {
				fontsErr = fmt.Errorf("parse %s: %w", name, err)
				return
			}
			fonts = append(fonts, face.Font)
		}
	})
	return fonts, fontsErr
}

// FontData returns the raw bytes of an embedded face, for renderers that
// rasterize outlines themselves or embed the font (SVG debug output).
func FontData(id draw.FontID) ([]byte, error) {
	if int(id) >= len(embeddedFiles) {
		return nil, fmt.Errorf("font %d is not embedded", id)
	}
	return fontFS.ReadFile(embeddedFiles[id])
}
