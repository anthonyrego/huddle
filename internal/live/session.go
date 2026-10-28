// Package live drives one Gemini Live session. go-genai's session has no
// lock on its websocket, so one writer goroutine owns every send and one
// reader goroutine turns server messages into Events, delivered in the
// order they arrived. Order matters: a tool call is stamped with how much
// audio came before it.
//
// Copied from an earlier project of the author's.
package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genai"
)

// DefaultModel is the Live model the spike targets.
const DefaultModel = "gemini-3.8-live"

// Config is what a session is set up with.
type Config struct {
	Model  string // DefaultModel when empty
	System string // the system instruction
	Voice  string // a prebuilt voice name; the model's default when empty
	Tools  []*genai.Tool

	// Resume is a handle from an earlier session's Resumable event.
	Resume string
	// CompressAt turns on context compression: when the context reaches
	// this many tokens the server trims it to CompressTo. Zero leaves the
	// session at the model's limits.
	CompressAt, CompressTo int64

	// EndOfSpeech is how readily the server decides you stopped talking:
	// "high" answers sooner, "low" waits through longer pauses, "" is the
	// server's default. SilenceMS is how much quiet it waits for (0: its
	// default). Both trade reply latency against cutting you off.
	EndOfSpeech string
	SilenceMS   int
	// StartOfSpeech is how readily the server decides you started talking
	// (and so interrupts a reply): "low" needs clearer speech, "high"
	// less, "" is the server's default. PrefixMS is how much speech it
	// needs before it commits to a start (0: its default). Both trade
	// barge-in speed against the model interrupting itself on echo.
	StartOfSpeech string
	PrefixMS      int

	// MediaResolution is how many tokens an image costs: "low", "medium"
	// or "high" (the same tokens, spent on a zoomed reframing), "" for the
	// server's default.
	MediaResolution string
	// Video makes every frame sent since the last turn part of the next
	// one. Without it a turn takes only what came while the person was
	// heard talking: a frame sent before typed text, or just after they
	// stop, is never seen.
	Video bool
}

// Kind says what an Event carries.
type Kind int

const (
	Audio              Kind = iota // Data: PCM at Rate
	InputText                      // Text: a piece of the transcript of the user
	OutputText                     // Text: a piece of the transcript of the model
	Interrupted                    // the user spoke over the model: drop queued audio
	GenerationComplete             // the model has sent all of this turn
	TurnComplete                   // the turn is over
	ToolCall                       // Calls
	ToolCancel                     // IDs: calls the model no longer wants
	GoAway                         // TimeLeft: the server will close the connection
	Resumable                      // Handle: reconnect with this to keep the conversation
	VoiceStart                     // the server heard the user start speaking
	VoiceEnd                       // the server heard the user stop
	Usage                          // Usage
	Closed                         // Err: the session ended (nil after Close)
)

var kindNames = [...]string{"audio", "input-text", "output-text", "interrupted", "generation-complete", "turn-complete", "tool-call", "tool-cancel", "go-away", "resumable", "voice-start", "voice-end", "usage", "closed"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Event is one thing the server said, stamped with when it arrived.
type Event struct {
	Kind     Kind
	At       time.Time
	Data     []byte
	Rate     int
	Text     string
	Calls    []*genai.FunctionCall
	IDs      []string
	TimeLeft time.Duration
	Handle   string
	Usage    *genai.UsageMetadata
	Err      error
}

// Session is a connected Live session.
type Session struct {
	gs     *genai.Session
	out    chan func(*genai.Session) error
	events chan Event

	done     chan struct{} // closed by Close, or when a send fails
	stopOnce sync.Once
	closing  atomic.Bool

	mu      sync.Mutex
	sendErr error // the send that failed, reported in Closed

	// Dropped counts audio chunks thrown away because the writer was behind.
	Dropped atomic.Int64
	// DroppedFrames counts images thrown away for the same reason.
	DroppedFrames atomic.Int64
}

// Connect opens a session. The context bounds only the dial and setup; the
// session lives until Close or the server ends it.
func Connect(ctx context.Context, apiKey string, cfg Config) (*Session, error) {
	if apiKey == "" {
		return nil, errors.New("no API key")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:      apiKey,
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{APIVersion: "v1beta"},
	})
	if err != nil {
		return nil, err
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	gs, err := client.Live.Connect(ctx, model, connectConfig(cfg))
	if err != nil {
		return nil, err
	}
	s := &Session{
		gs:     gs,
		out:    make(chan func(*genai.Session) error, 64),
		events: make(chan Event, 256),
		done:   make(chan struct{}),
	}
	go s.write()
	go s.read()
	return s, nil
}

func connectConfig(cfg Config) *genai.LiveConnectConfig {
	c := &genai.LiveConnectConfig{
		ResponseModalities:       []genai.Modality{genai.ModalityAudio},
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
		Tools:                    cfg.Tools,
		SessionResumption:        &genai.SessionResumptionConfig{Handle: cfg.Resume},
	}
	if cfg.System != "" {
		c.SystemInstruction = genai.NewContentFromText(cfg.System, genai.RoleUser)
	}
	if cfg.Voice != "" {
		c.SpeechConfig = &genai.SpeechConfig{VoiceConfig: &genai.VoiceConfig{
			PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: cfg.Voice},
		}}
	}
	if cfg.Video {
		c.RealtimeInputConfig = &genai.RealtimeInputConfig{TurnCoverage: genai.TurnCoverageTurnIncludesAudioActivityAndAllVideo}
	}
	if cfg.EndOfSpeech != "" || cfg.SilenceMS > 0 || cfg.StartOfSpeech != "" || cfg.PrefixMS > 0 {
		vad := &genai.AutomaticActivityDetection{}
		switch cfg.StartOfSpeech {
		case "high":
			vad.StartOfSpeechSensitivity = genai.StartSensitivityHigh
		case "low":
			vad.StartOfSpeechSensitivity = genai.StartSensitivityLow
		}
		if cfg.PrefixMS > 0 {
			vad.PrefixPaddingMs = genai.Ptr(int32(cfg.PrefixMS))
		}
		switch cfg.EndOfSpeech {
		case "high":
			vad.EndOfSpeechSensitivity = genai.EndSensitivityHigh
		case "low":
			vad.EndOfSpeechSensitivity = genai.EndSensitivityLow
		}
		if cfg.SilenceMS > 0 {
			vad.SilenceDurationMs = genai.Ptr(int32(cfg.SilenceMS))
		}
		if c.RealtimeInputConfig == nil {
			c.RealtimeInputConfig = &genai.RealtimeInputConfig{}
		}
		c.RealtimeInputConfig.AutomaticActivityDetection = vad
	}
	switch cfg.MediaResolution {
	case "low":
		c.MediaResolution = genai.MediaResolutionLow
	case "medium":
		c.MediaResolution = genai.MediaResolutionMedium
	case "high":
		c.MediaResolution = genai.MediaResolutionHigh
	}
	if cfg.CompressAt > 0 {
		c.ContextWindowCompression = &genai.ContextWindowCompressionConfig{
			TriggerTokens: genai.Ptr(cfg.CompressAt),
			SlidingWindow: &genai.SlidingWindow{TargetTokens: genai.Ptr(cfg.CompressTo)},
		}
	}
	return c
}

// Events delivers what the server says, in order. It is closed after the
// Closed event.
func (s *Session) Events() <-chan Event { return s.events }

// SendAudio queues one chunk of microphone PCM at rate. It never blocks: if
// the writer is behind, the chunk is dropped and counted in Dropped.
func (s *Session) SendAudio(pcm []byte, rate int) {
	mime := "audio/pcm;rate=" + strconv.Itoa(rate)
	send := func(gs *genai.Session) error {
		return gs.SendRealtimeInput(genai.LiveRealtimeInput{Audio: &genai.Blob{Data: pcm, MIMEType: mime}})
	}
	select {
	case <-s.done:
	case s.out <- send:
	default:
		s.Dropped.Add(1)
	}
}

// SendImage queues one JPEG frame of what the user sees, as realtime
// video. Like SendAudio it never blocks: a stale frame is worth nothing,
// so if the writer is behind it is dropped and counted in DroppedFrames.
func (s *Session) SendImage(jpeg []byte) {
	send := func(gs *genai.Session) error {
		return gs.SendRealtimeInput(genai.LiveRealtimeInput{Video: &genai.Blob{Data: jpeg, MIMEType: "image/jpeg"}})
	}
	select {
	case <-s.done:
	case s.out <- send:
	default:
		s.DroppedFrames.Add(1)
	}
}

// SendText adds text to the realtime input, alongside the audio. The board
// uses it to say what the user is pointing at.
func (s *Session) SendText(text string) {
	s.enqueue(func(gs *genai.Session) error {
		return gs.SendRealtimeInput(genai.LiveRealtimeInput{Text: text})
	})
}

// SendToolResponse answers tool calls.
func (s *Session) SendToolResponse(rs []*genai.FunctionResponse) {
	s.enqueue(func(gs *genai.Session) error {
		return gs.SendToolResponse(genai.LiveToolResponseInput{FunctionResponses: rs})
	})
}

// enqueue waits for room, or gives up once the session is ending.
func (s *Session) enqueue(f func(*genai.Session) error) {
	select {
	case <-s.done:
	case s.out <- f:
	}
}

func (s *Session) stop() { s.stopOnce.Do(func() { close(s.done) }) }

// Close ends the session. The Events channel then delivers Closed with a
// nil error and closes.
func (s *Session) Close() error {
	s.closing.Store(true)
	s.stop()
	return s.gs.Close()
}

func (s *Session) write() {
	for {
		select {
		case <-s.done:
			return
		case f := <-s.out:
			if err := f(s.gs); err != nil {
				s.mu.Lock()
				s.sendErr = fmt.Errorf("send: %w", err)
				s.mu.Unlock()
				s.stop()
				s.gs.Close() // wakes the reader, which reports the error
				return
			}
		}
	}
}

func (s *Session) read() {
	defer close(s.events)
	for {
		msg, err := s.gs.Receive()
		if err != nil {
			s.mu.Lock()
			if s.sendErr != nil {
				err = s.sendErr
			} else if s.closing.Load() {
				err = nil
			}
			s.mu.Unlock()
			s.stop()
			s.events <- Event{Kind: Closed, At: time.Now(), Err: err}
			return
		}
		for _, ev := range translate(msg, time.Now()) {
			s.events <- ev
		}
	}
}

// translate turns one server message into events, in the order the client
// should act on them: audio first, then transcripts, then the end of a turn.
func translate(m *genai.LiveServerMessage, now time.Time) []Event {
	var evs []Event
	add := func(e Event) { e.At = now; evs = append(evs, e) }
	if c := m.ServerContent; c != nil {
		if c.ModelTurn != nil {
			for _, p := range c.ModelTurn.Parts {
				if p.InlineData != nil && strings.HasPrefix(p.InlineData.MIMEType, "audio/") {
					add(Event{Kind: Audio, Data: p.InlineData.Data, Rate: rateOf(p.InlineData.MIMEType)})
				} else if p.Text != "" {
					slog.Debug("live: model text", "thought", p.Thought, "text", p.Text)
				}
			}
		}
		if t := c.InputTranscription; t != nil && t.Text != "" {
			add(Event{Kind: InputText, Text: t.Text})
		}
		if t := c.OutputTranscription; t != nil && t.Text != "" {
			add(Event{Kind: OutputText, Text: t.Text})
		}
		if c.Interrupted {
			add(Event{Kind: Interrupted})
		}
		if c.GenerationComplete {
			add(Event{Kind: GenerationComplete})
		}
		if c.TurnComplete {
			add(Event{Kind: TurnComplete})
		}
	}
	if m.ToolCall != nil && len(m.ToolCall.FunctionCalls) > 0 {
		add(Event{Kind: ToolCall, Calls: m.ToolCall.FunctionCalls})
	}
	if m.ToolCallCancellation != nil && len(m.ToolCallCancellation.IDs) > 0 {
		add(Event{Kind: ToolCancel, IDs: m.ToolCallCancellation.IDs})
	}
	if v := m.VoiceActivity; v != nil {
		switch v.VoiceActivityType {
		case genai.VoiceActivityTypeActivityStart:
			add(Event{Kind: VoiceStart})
		case genai.VoiceActivityTypeActivityEnd:
			add(Event{Kind: VoiceEnd})
		}
	}
	if m.GoAway != nil {
		add(Event{Kind: GoAway, TimeLeft: m.GoAway.TimeLeft})
	}
	if u := m.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
		add(Event{Kind: Resumable, Handle: u.NewHandle})
	}
	if m.UsageMetadata != nil {
		add(Event{Kind: Usage, Usage: m.UsageMetadata})
	}
	return evs
}

// rateOf reads the sample rate from a MIME type like "audio/pcm;rate=24000",
// defaulting to Live's output rate.
func rateOf(mime string) int {
	for _, param := range strings.Split(mime, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(param), "rate="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 24000
}
