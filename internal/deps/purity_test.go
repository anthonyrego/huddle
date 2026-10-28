// Copied from an earlier project of the author's.
package deps

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/anthonyrego/huddle"

// pure lists directories (relative to the repo root) whose packages must never
// depend on SDL: the core runs in the server, the CLI and tests without a GPU.
// A listed directory that doesn't exist yet is skipped; its subpackages are
// included.
var pure = []string{
	"internal/board",
	"internal/config",
	"internal/cue",
	"internal/draw",
	"internal/fixturemeet",
	"internal/gate",
	"internal/geom",
	"internal/jitter",
	"internal/live",
	"internal/logx",
	"internal/opus",
	"internal/pcm",
	"internal/playout",
	"internal/rtc",
	"internal/server",
	"internal/text",
	"internal/theme",
	"internal/ui",
	"internal/wire",
}

// TestCoreDoesNotImportSDL walks imports in-process with go/build (so the
// test cache sees every file it reads) and fails if a pure package reaches
// go-sdl3 through any chain of this module's packages.
func TestCoreDoesNotImportSDL(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	for _, dir := range pure {
		filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				if rel, _ := filepath.Rel(root, p); rel != "" {
					starts = append(starts, module+"/"+filepath.ToSlash(rel))
				}
			}
			return nil
		})
	}

	seen := map[string]bool{}
	var visit func(path string, chain []string)
	visit = func(path string, chain []string) {
		if seen[path] {
			return
		}
		seen[path] = true
		dir := filepath.Join(root, strings.TrimPrefix(path, module))
		pkg, err := build.ImportDir(dir, 0)
		if err != nil {
			return // no Go files (e.g. a fonts directory)
		}
		chain = append(chain, path)
		for _, imp := range pkg.Imports {
			switch {
			case strings.HasPrefix(imp, "github.com/Zyko0/go-sdl3"):
				t.Errorf("pure package imports %s via %s", imp, strings.Join(chain, " -> "))
			case strings.HasPrefix(imp, module+"/"):
				visit(imp, chain)
			}
		}
	}
	for _, s := range starts {
		visit(s, nil)
	}
	if len(seen) < 5 {
		t.Fatalf("walked only %d packages; is the repo root right (%s)?", len(seen), root)
	}
}
