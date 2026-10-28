// Package fixturemeet stages meetings for the whiteboard from scripts:
// each speaker's lines rendered with a macOS voice into an Ogg Opus track
// (Render), and a bot per speaker that joins the room and plays it in
// real time (Play), so an agent can be scored against what each line was
// meant to be.
//
// A script (testdata/meetings/*.txt) is one line per utterance:
//
//	# comments
//	@voice Sam Samantha     the macOS voice a speaker talks with
//	@gap 0.8                seconds of quiet between lines (default 0.8)
//	@pause 3                seconds of quiet before the next line
//	@repeat 12 … @end       the lines between, n times; {n} is the round
//	Sam: So the release has three parts.
//	Sam [command]: Whiteboard, draw our release process.
//	& Kim [command]: Whiteboard, add hiring.   (& starts with the line before)
//
// Labels: command (the whiteboard should draw), question (it should
// answer aloud), stop (it should stop), distractor (the board is talked
// about, not to); a line without one is talk.
package fixturemeet

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Labels a line can have.
const (
	Talk       = "talk"
	Command    = "command"
	Question   = "question"
	Stop       = "stop"
	Distractor = "distractor"
)

// Line is one utterance.
type Line struct {
	Speaker string
	Label   string
	Text    string
	With    bool    // starts with the line before
	Pause   float64 // seconds of quiet before it
}

// Addressed reports whether the line is said to the whiteboard.
func (l Line) Addressed() bool { return l.Label == Command || l.Label == Question || l.Label == Stop }

// Script is a parsed meeting.
type Script struct {
	Name   string
	Voices map[string]string
	Lines  []Line
}

// Speakers are the script's speakers in the order they first talk.
func (s *Script) Speakers() []string {
	var out []string
	for _, l := range s.Lines {
		if !slices.Contains(out, l.Speaker) {
			out = append(out, l.Speaker)
		}
	}
	return out
}

// voices are given to speakers with no @voice, in order.
var voices = []string{"Samantha", "Daniel", "Karen", "Moira", "Rishi", "Tessa", "Fred"}

// Parse reads a script.
func Parse(path string) (*Script, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &Script{Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), Voices: map[string]string{}}
	gap := 0.8
	var pause float64
	var block []Line // inside @repeat
	repeat := 0
	var pre float64 // the pause before a block
	sc := bufio.NewScanner(f)
	add := func(l Line) {
		if repeat > 0 {
			block = append(block, l)
			return
		}
		s.Lines = append(s.Lines, l)
	}
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bad := func(why string) error { return fmt.Errorf("%s:%d: %s", path, n, why) }
		if strings.HasPrefix(line, "@") {
			f := strings.Fields(line)
			switch f[0] {
			case "@voice":
				if len(f) != 3 {
					return nil, bad("@voice NAME VOICE")
				}
				s.Voices[f[1]] = f[2]
			case "@gap", "@pause":
				if len(f) != 2 {
					return nil, bad(f[0] + " SECONDS")
				}
				v, err := strconv.ParseFloat(f[1], 64)
				if err != nil {
					return nil, bad(err.Error())
				}
				if f[0] == "@gap" {
					gap = v
				} else {
					pause += v
				}
			case "@repeat":
				if len(f) != 2 || repeat > 0 {
					return nil, bad("@repeat N (not nested)")
				}
				if repeat, err = strconv.Atoi(f[1]); err != nil || repeat < 1 {
					return nil, bad("@repeat N")
				}
				pre, pause = pause, 0
			case "@end":
				if repeat == 0 {
					return nil, bad("@end without @repeat")
				}
				// A pause at the end of the block comes before each round
				// after the first, and before the line after the block.
				tail := pause
				for r := 1; r <= repeat; r++ {
					for i, l := range block {
						l.Text = strings.ReplaceAll(l.Text, "{n}", strconv.Itoa(r))
						switch {
						case i == 0 && r == 1:
							l.Pause += pre
						case i == 0:
							l.Pause += tail
						}
						s.Lines = append(s.Lines, l)
					}
				}
				repeat, block = 0, nil
			default:
				return nil, bad("unknown directive " + f[0])
			}
			continue
		}
		l := Line{Label: Talk}
		if rest, ok := strings.CutPrefix(line, "&"); ok {
			l.With, line = true, strings.TrimSpace(rest)
		}
		head, text, ok := strings.Cut(line, ":")
		if !ok {
			return nil, bad("want SPEAKER [label]: text")
		}
		head = strings.TrimSpace(head)
		if i := strings.IndexByte(head, '['); i >= 0 && strings.HasSuffix(head, "]") {
			l.Label = strings.TrimSpace(head[i+1 : len(head)-1])
			head = strings.TrimSpace(head[:i])
		}
		switch l.Label {
		case Talk, Command, Question, Stop, Distractor:
		default:
			return nil, bad("unknown label " + l.Label)
		}
		l.Speaker, l.Text = head, strings.TrimSpace(text)
		if l.Speaker == "" || l.Text == "" {
			return nil, bad("a line needs a speaker and words")
		}
		l.Pause = gap + pause
		pause = 0
		add(l)
	}
	if repeat > 0 {
		return nil, fmt.Errorf("%s: @repeat without @end", path)
	}
	i := 0
	for _, sp := range s.Speakers() {
		if s.Voices[sp] == "" {
			for slices.Contains(mapValues(s.Voices), voices[i%len(voices)]) && i < len(voices) {
				i++
			}
			s.Voices[sp] = voices[i%len(voices)]
			i++
		}
	}
	return s, sc.Err()
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
