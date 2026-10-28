// Command shadermanifest records and checks shaders/manifest.json: the sha256
// of every shader source and compiled output, plus the tool versions that
// built them. CI runs "check" so a source edited without rebuilding (or an
// output edited by hand) fails, without CI needing the shader toolchain.
//
//	go run ./scripts/shadermanifest write   # after scripts/build-shaders.sh
//	go run ./scripts/shadermanifest check
//
// Copied from an earlier project of the author's.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const dir = "shaders"

type manifest struct {
	Tools   map[string]string `json:"tools"`
	Shaders []entry           `json:"shaders"`
}

type entry struct {
	Source  string            `json:"source"`
	SHA256  string            `json:"sha256"`
	Outputs map[string]string `json:"outputs"`
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "write" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: shadermanifest write|check")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "shadermanifest:", err)
		os.Exit(1)
	}
}

func run(mode string) error {
	path := filepath.Join(dir, "manifest.json")
	var old manifest
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &old); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if mode == "check" {
		return err
	}

	cur := manifest{Tools: old.Tools}
	if mode == "write" {
		cur.Tools = map[string]string{
			"glslang":     os.Getenv("GLSLANG_VERSION"),
			"spirv-cross": os.Getenv("SPIRV_CROSS_VERSION"),
		}
	}
	sources, err := filepath.Glob(filepath.Join(dir, "src", "*.hlsl"))
	if err != nil {
		return err
	}
	sort.Strings(sources)
	for _, src := range sources {
		sum, err := hash(src)
		if err != nil {
			return err
		}
		stem := strings.TrimSuffix(filepath.Base(src), ".hlsl")
		e := entry{Source: rel(src), SHA256: sum, Outputs: map[string]string{}}
		for _, out := range []string{
			filepath.Join(dir, "compiled", "spirv", stem+".spv"),
			filepath.Join(dir, "compiled", "msl", stem+".msl"),
		} {
			s, err := hash(out)
			if err != nil {
				return fmt.Errorf("%s has no compiled output %s (run scripts/build-shaders.sh)", rel(src), rel(out))
			}
			e.Outputs[rel(out)] = s
		}
		cur.Shaders = append(cur.Shaders, e)
	}

	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if mode == "write" {
		return os.WriteFile(path, b, 0o644)
	}
	oldB, _ := os.ReadFile(path)
	if !bytes.Equal(oldB, b) {
		return fmt.Errorf("%s is stale: shader sources or outputs changed without scripts/build-shaders.sh", path)
	}
	fmt.Println("shaders: manifest matches sources and outputs")
	return nil
}

func hash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func rel(p string) string {
	return filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
}
