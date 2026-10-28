package ui

import (
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
)

// DrawText appends a text layout with its top-left corner at at.
func DrawText(l *draw.List, lay text.Layout, at draw.Point) {
	for _, line := range lay.Lines {
		for _, run := range line.Runs {
			run.Origin.X += at.X
			run.Origin.Y += at.Y
			l.Glyphs(run)
		}
	}
}
