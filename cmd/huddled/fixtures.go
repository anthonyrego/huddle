package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/anthonyrego/huddle/internal/fixturemeet"
)

func init() {
	register(&command{name: "fixtures", summary: "render fixture meetings (testdata/meetings/*.txt) to per-speaker Ogg Opus with macOS voices", run: runFixtures})
}

func runFixtures(args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	out := fs.String("out", "testdata/meetings", "where the audio and timing go")
	cache := fs.String("cache", ".dev/say-cache", "where rendered lines are kept")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scripts := fs.Args()
	if len(scripts) == 0 {
		var err error
		if scripts, err = filepath.Glob("testdata/meetings/*.txt"); err != nil || len(scripts) == 0 {
			return errors.New("no scripts given or in testdata/meetings")
		}
	}
	for _, p := range scripts {
		start := time.Now()
		s, err := fixturemeet.Parse(p)
		if err != nil {
			return err
		}
		t, err := fixturemeet.Render(s, *out, *cache)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		n := 0
		for _, l := range t.Lines {
			if l.Label != fixturemeet.Talk {
				n++
			}
		}
		fmt.Printf("%s: %d lines (%d labeled), %d speakers, %s long, rendered in %s\n", s.Name, len(t.Lines), n,
			len(s.Speakers()), (time.Duration(t.DurationMs) * time.Millisecond).Round(time.Second), time.Since(start).Round(time.Millisecond))
	}
	return nil
}
