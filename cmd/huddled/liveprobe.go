package main

// live-probe: what the whiteboard's design rests on, asked of the real
// models. The brain probe sends commands to the realtime model as client
// content and as realtime text, alternating, and records for each turn
// whether tool calls came, when, whether a speak call came before the
// audio, and how much audio came; one question answers speak WHEN_IDLE
// and one SILENT. The transcription probe streams speech rendered with
// macOS say, at 16 kHz in 100 ms chunks, in real time, and records the
// interim and final transcripts against when the speech ended.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/live"
	"github.com/anthonyrego/huddle/internal/pcm"
)

func init() {
	register(&command{name: "live-probe", summary: "probe the realtime and transcription models the whiteboard relies on (spends a few cents)", run: runLiveProbe})
}

func runLiveProbe(args []string) error {
	fs := flag.NewFlagSet("live-probe", flag.ContinueOnError)
	model := fs.String("model", live.DefaultModel, "the realtime model")
	tmodel := fs.String("transcribe-model", "gemini-3.5-transcribe-live", "the transcription model")
	only := fs.String("only", "", "brain or transcribe (default: both)")
	fs.BoolVar(autoSched, "auto-schedule", false, "answer drawing calls SILENT and only a speak, an error or a read WHEN_IDLE (instead of each turn's fixed scheduling)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return errors.New("GEMINI_API_KEY is not set (make live-probe reads it from .env)")
	}
	ctx := context.Background()
	if *only == "" || *only == "brain" {
		if err := probeBrain(ctx, key, *model); err != nil {
			return fmt.Errorf("brain: %w", err)
		}
	}
	if *only == "" || *only == "transcribe" {
		if err := probeTranscribe(ctx, key, *tmodel); err != nil {
			return fmt.Errorf("transcribe: %w", err)
		}
	}
	return nil
}

const probeSystem = `You are the whiteboard in a meeting. Every user turn is one command from a named person.
Draw with the board tools. Only when the command is a direct question that needs a spoken answer,
call speak first, then answer aloud in one short sentence. For drawing commands, draw and stay silent.`

var autoSched = new(bool)

var probeSpeak = &genai.FunctionDeclaration{
	Name:        "speak",
	Description: "Call this before you answer aloud, and only when the command is a question that needs a spoken answer.",
	Behavior:    genai.BehaviorNonBlocking,
}

type probeTurn struct {
	via   string // client or realtime
	text  string
	speak genai.FunctionResponseScheduling // how speak is answered
}

func probeBrain(ctx context.Context, key, model string) error {
	tools := board.Tools()
	tools[0].FunctionDeclarations = append(tools[0].FunctionDeclarations, probeSpeak)
	s, err := live.Connect(ctx, key, live.Config{Model: model, System: probeSystem, Tools: tools})
	if err != nil {
		return err
	}
	defer s.Close()
	turns := []probeTurn{
		{"client", "Sam says: whiteboard, draw our release process: build, then test, then deploy.", ""},
		{"realtime", "Kim says: whiteboard, add a code review step between test and deploy.", ""},
		{"client", "Dan asks: whiteboard, how many steps are on the board now?", genai.FunctionResponseSchedulingWhenIdle},
		{"realtime", "Sam says: whiteboard, put review and deploy in a group called shipping.", ""},
		{"realtime", "Kim asks: whiteboard, which step comes first?", genai.FunctionResponseSchedulingSilent},
		{"client", "Dan says: whiteboard, add a monitoring step after deploy.", ""},
		{"realtime", "Sam asks: whiteboard, what comes right after review?", genai.FunctionResponseSchedulingWhenIdle},
		{"client", "Kim says: whiteboard, add a rollback step after monitoring, and a canary step between review and deploy.", genai.FunctionResponseSchedulingSilent},
		{"realtime", "Dan says: whiteboard, connect rollback back to the qa gate.", ""},
		{"client", "Sam says: whiteboard, group build and test as making it, and note that tests take ten minutes.", ""},
	}
	doc := board.New()
	var prompt, response int32
	for i, t := range turns {
		start := time.Now()
		if t.via == "client" {
			s.SendTurn(t.text, true)
		} else {
			s.SendText(t.text)
		}
		var firstCall, speakAt, firstAudio, done time.Duration
		var calls []string
		var audio, completes int
		var said strings.Builder
		var timeline []string
		timeout := time.After(45 * time.Second)
		// A turn is over once a turn-complete is followed by quiet: after
		// tool responses the model often starts another generation.
		var quiet <-chan time.Time
	loop:
		for {
			select {
			case <-timeout:
				fmt.Printf("  turn %d: timed out\n", i+1)
				break loop
			case <-quiet:
				break loop
			case ev, ok := <-s.Events():
				if !ok {
					return errors.New("session closed")
				}
				at := time.Since(start)
				if ev.Kind != live.Usage {
					quiet = nil
				}
				switch ev.Kind {
				case live.ToolCall:
					if firstCall == 0 {
						firstCall = at
					}
					var rs []*genai.FunctionResponse
					var names []string
					failed := false
					for _, c := range ev.Calls {
						calls = append(calls, c.Name)
						names = append(names, c.Name)
						var r *genai.FunctionResponse
						if c.Name == "speak" {
							if speakAt == 0 {
								speakAt = at
							}
							r = &genai.FunctionResponse{ID: c.ID, Name: c.Name, Scheduling: genai.FunctionResponseSchedulingWhenIdle, Response: map[string]any{"ok": true}}
						} else {
							var err error
							_, r, err = board.Answer(doc, c)
							failed = failed || err != nil || c.Name == board.GetBoard
						}
						if t.speak != "" && !*autoSched {
							r.Scheduling = t.speak
						}
						rs = append(rs, r)
					}
					if *autoSched {
						// Drawing that went through needs no reply from the
						// model; a speak, an error or a read does.
						sched := genai.FunctionResponseSchedulingSilent
						if speakAt != 0 || failed {
							sched = genai.FunctionResponseSchedulingWhenIdle
						}
						for _, r := range rs {
							r.Scheduling = sched
						}
						names = append(names, "→"+string(sched))
					}
					timeline = append(timeline, fmt.Sprintf("%v calls %v", ms(at), names))
					s.SendToolResponse(rs)
				case live.Audio:
					if firstAudio == 0 {
						firstAudio = at
						timeline = append(timeline, fmt.Sprintf("%v first audio", ms(at)))
					}
					audio += len(ev.Data)
				case live.OutputText:
					said.WriteString(ev.Text)
				case live.Usage:
					prompt += ev.Usage.PromptTokenCount
					response += ev.Usage.ResponseTokenCount
				case live.TurnComplete:
					done = at
					completes++
					timeline = append(timeline, fmt.Sprintf("%v turn-complete", ms(at)))
					quiet = time.After(3 * time.Second)
				case live.GenerationComplete:
					timeline = append(timeline, fmt.Sprintf("%v generation-complete", ms(at)))
				case live.Interrupted:
					timeline = append(timeline, fmt.Sprintf("%v interrupted", ms(at)))
				case live.Closed:
					return fmt.Errorf("closed: %v", ev.Err)
				}
			}
		}
		audioS := float64(audio) / (24000 * 2)
		fmt.Printf("turn %d via %-8s calls=%v first-call=%v speak=%v first-audio=%v audio=%.1fs last-complete=%v completes=%d\n    %q\n    said: %q\n    %s\n",
			i+1, t.via, calls, ms(firstCall), ms(speakAt), ms(firstAudio), audioS, ms(done), completes, t.text,
			strings.TrimSpace(said.String()), strings.Join(timeline, " | "))
		if t.speak != "" {
			fmt.Printf("    every call answered %s\n", t.speak)
		}
	}
	fmt.Printf("board after the probe:\n%s\n", doc.Digest())
	fmt.Printf("usage: prompt %d tokens, response %d tokens\n", prompt, response)
	return nil
}

func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}

// sayWAV renders text with a macOS voice as 16 kHz mono s16.
func sayWAV(dir, name, voice, text string) ([]byte, error) {
	path := filepath.Join(dir, name+".wav")
	cmd := exec.Command("say", "-v", voice, "--file-format=WAVE", "--data-format=LEI16@16000", "-o", path, text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("say: %v: %s", err, out)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, rate, err := pcm.ReadWAV(f)
	if err != nil {
		return nil, err
	}
	if rate != 16000 {
		return nil, fmt.Errorf("say gave %d Hz", rate)
	}
	return data, nil
}

func probeTranscribe(ctx context.Context, key, model string) error {
	dir, err := os.MkdirTemp("", "huddle-probe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	utts := []struct {
		voice, text string
		end         bool // send audioStreamEnd when it ends (hybrid VAD)
	}{
		{"Samantha", "Whiteboard, draw our release process: build, then test, then deploy.", true},
		{"Daniel", "I'll put it on the whiteboard later, after lunch.", false},
		{"Karen", "White board, add a code review step between test and deploy.", true},
	}
	s, err := live.Connect(ctx, key, live.Config{Model: model, Text: true})
	if err != nil {
		return err
	}
	defer s.Close()
	const rate, chunk = 16000, 3200 // 100 ms
	silence := make([]byte, chunk)
	start := time.Now()
	events := make(chan live.Event, 256)
	go func() {
		for ev := range s.Events() {
			events <- ev
		}
		close(events)
	}()
	var ended time.Time
	report := func(ev live.Event) {
		at := time.Since(start)
		rel := ""
		if !ended.IsZero() {
			rel = fmt.Sprintf(" (%+v after speech ended)", time.Since(ended).Round(time.Millisecond))
		}
		switch ev.Kind {
		case live.InterimText:
			fmt.Printf("  %7v interim%s: %q\n", at.Round(time.Millisecond), rel, ev.Text)
		case live.InputText:
			fmt.Printf("  %7v FINAL%s finished=%v: %q\n", at.Round(time.Millisecond), rel, ev.Finished, ev.Text)
		case live.ModelText:
			fmt.Printf("  %7v model text%s: %q\n", at.Round(time.Millisecond), rel, ev.Text)
		case live.Usage:
			fmt.Printf("  %7v usage: prompt %d response %d\n", at.Round(time.Millisecond), ev.Usage.PromptTokenCount, ev.Usage.ResponseTokenCount)
		case live.Closed:
			fmt.Printf("  %7v closed: %v\n", at.Round(time.Millisecond), ev.Err)
		default:
			fmt.Printf("  %7v %s%s\n", at.Round(time.Millisecond), ev.Kind, rel)
		}
	}
	pump := func(d time.Duration) {
		until := time.Now().Add(d)
		for time.Now().Before(until) {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				report(ev)
			case <-time.After(time.Until(until)):
			}
		}
	}
	for i, u := range utts {
		data, err := sayWAV(dir, fmt.Sprint(i), u.voice, u.text)
		if err != nil {
			return err
		}
		fmt.Printf("utterance %d (%s, %.1fs, stream end=%v): %q\n", i+1, u.voice, float64(len(data))/(rate*2), u.end, u.text)
		next := time.Now()
		for off := 0; off < len(data); off += chunk {
			b := data[off:min(off+chunk, len(data))]
			s.SendAudio(append([]byte(nil), b...), rate)
			next = next.Add(100 * time.Millisecond)
			pump(time.Until(next))
		}
		ended = time.Now()
		if u.end {
			s.SendAudioEnd()
			pump(3 * time.Second)
		} else {
			// Server VAD only: keep streaming silence.
			for k := 0; k < 30; k++ {
				s.SendAudio(silence, rate)
				pump(100 * time.Millisecond)
			}
		}
		ended = time.Time{}
	}
	pump(2 * time.Second)
	return nil
}
