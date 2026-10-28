// Package shaders embeds the compiled shaders. Sources are in src/; run
// scripts/build-shaders.sh after editing them (see manifest.json).
//
// Copied from an earlier project of the author's.
package shaders

import "embed"

//go:embed compiled/spirv/*.spv compiled/msl/*.msl
var FS embed.FS
