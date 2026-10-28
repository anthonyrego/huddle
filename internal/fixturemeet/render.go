package fixturemeet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/anthonyrego/huddle/internal/pcm"
)

// Rate is the fixtures' sample rate.
const Rate = 48000

// overlapLead is how far after the line before a line marked & starts.
const overlapLead = 0.25

// Timing is where each line of a rendered meeting falls.
type Timing struct {
	Name       string            `json:"name"`
	DurationMs int64             `json:"duration_ms"`
	Voices     map[string]string `json:"voices"`
	Lines      []TimedLine       `json:"lines"`
}

// TimedLine is a line and when it is said, in ms from the start.
type TimedLine struct {
	Speaker string `json:"speaker"`
	Label   string `json:"label"`
	Text    string `json:"text"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	With    bool   `json:"with,omitempty"`
}

// Paths are where a rendered meeting's files go.
func Paths(dir, name, speaker string) (ogg, timing string) {
	return filepath.Join(dir, name+"."+speaker+".ogg"), filepath.Join(dir, name+".timing.json")
}

// ReadTiming reads a rendered meeting's timing.
func ReadTiming(dir, name string) (*Timing, error) {
	_, p := Paths(dir, name, "")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%w (make fixtures renders it)", err)
	}
	var t Timing
	return &t, json.Unmarshal(b, &t)
}

// Render says every line with its speaker's voice (macOS say, cached in
// cache by voice and words), lays the lines out in time, and writes each
// speaker's track as 48 kHz mono Ogg Opus with ffmpeg, plus the timing.
func Render(s *Script, dir, cache string) (*Timing, error) {
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	t := &Timing{Name: s.Name, Voices: s.Voices}
	clips := make([][]byte, len(s.Lines))
	var cursor, prevStart float64
	for i, l := range s.Lines {
		clip, err := say(cache, s.Voices[l.Speaker], l.Text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		clips[i] = clip
		dur := float64(len(clip)/2) / Rate
		start := cursor + l.Pause
		if l.With && i > 0 {
			start = prevStart + overlapLead
		}
		for _, p := range t.Lines {
			if p.Speaker == l.Speaker && int64(start*1000) < p.EndMs {
				return nil, fmt.Errorf("line %d: %s talks over themselves", i+1, l.Speaker)
			}
		}
		t.Lines = append(t.Lines, TimedLine{Speaker: l.Speaker, Label: l.Label, Text: l.Text,
			StartMs: int64(start * 1000), EndMs: int64((start + dur) * 1000), With: l.With})
		prevStart = start
		cursor = max(cursor, start+dur)
	}
	t.DurationMs = int64((cursor + 2) * 1000)
	for _, sp := range s.Speakers() {
		ogg, _ := Paths(dir, s.Name, sp)
		if err := writeTrack(ogg, t, clips, sp); err != nil {
			return nil, fmt.Errorf("%s: %w", sp, err)
		}
	}
	_, tp := Paths(dir, s.Name, "")
	b, _ := json.MarshalIndent(t, "", "  ")
	return t, os.WriteFile(tp, b, 0o644)
}

// say renders words as 48 kHz mono s16, cached.
func say(cache, voice, text string) ([]byte, error) {
	h := sha256.Sum256([]byte(voice + "\x00" + text))
	p := filepath.Join(cache, hex.EncodeToString(h[:12])+".wav")
	if _, err := os.Stat(p); err != nil {
		tmp := p + ".tmp.wav"
		out, err := exec.Command("say", "-v", voice, "--file-format=WAVE", "--data-format=LEI16@48000", "-o", tmp, text).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("say -v %s: %v: %s", voice, err, out)
		}
		if err := os.Rename(tmp, p); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, rate, err := pcm.ReadWAV(f)
	if err != nil {
		return nil, err
	}
	if rate != Rate {
		return nil, fmt.Errorf("say gave %d Hz", rate)
	}
	return data, nil
}

// writeTrack streams one speaker's lines, with quiet between, through
// ffmpeg into Ogg Opus.
func writeTrack(path string, t *Timing, clips [][]byte, speaker string) error {
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "-",
		"-c:a", "libopus", "-b:a", "32k", "-application", "voip", "-frame_duration", "20", path)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out := &limited{}
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return err
	}
	type piece struct {
		at   int64 // sample
		clip []byte
	}
	var pieces []piece
	for i, l := range t.Lines {
		if l.Speaker == speaker {
			pieces = append(pieces, piece{l.StartMs * Rate / 1000, clips[i]})
		}
	}
	slices.SortFunc(pieces, func(a, b piece) int { return int(a.at - b.at) })
	quiet := make([]byte, Rate/10*2)
	var pos int64
	writeQuiet := func(n int64) error {
		for n > 0 {
			k := min(n, int64(len(quiet)/2))
			if _, err := in.Write(quiet[:k*2]); err != nil {
				return err
			}
			n -= k
		}
		return nil
	}
	for _, p := range pieces {
		if err := writeQuiet(p.at - pos); err != nil {
			return err
		}
		if _, err := in.Write(p.clip); err != nil {
			return err
		}
		pos = p.at + int64(len(p.clip)/2)
	}
	if err := writeQuiet(t.DurationMs*Rate/1000 - pos); err != nil {
		return err
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, out.b)
	}
	return nil
}

// limited keeps the first few KB written to it.
type limited struct{ b []byte }

func (l *limited) Write(p []byte) (int, error) {
	if len(l.b) < 4096 {
		l.b = append(l.b, p[:min(len(p), 4096-len(l.b))]...)
	}
	return len(p), nil
}

var _ io.Writer = (*limited)(nil)

// Addressed reports whether the line is said to the whiteboard.
func (l TimedLine) Addressed() bool {
	return l.Label == Command || l.Label == Question || l.Label == Stop
}
