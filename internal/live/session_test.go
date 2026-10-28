package live

import (
	"testing"
	"time"

	"google.golang.org/genai"
)

func kinds(evs []Event) []Kind {
	ks := make([]Kind, len(evs))
	for i, e := range evs {
		ks[i] = e.Kind
	}
	return ks
}

func TestTranslateOrdersAudioBeforeTurnEnd(t *testing.T) {
	now := time.Unix(10, 0)
	m := &genai.LiveServerMessage{ServerContent: &genai.LiveServerContent{
		ModelTurn: &genai.Content{Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "audio/pcm;rate=24000", Data: []byte{1, 2}}},
			{Text: "planning the reply", Thought: true},
			{InlineData: &genai.Blob{MIMEType: "audio/pcm", Data: []byte{3, 4}}},
		}},
		OutputTranscription: &genai.Transcription{Text: "Hello"},
		GenerationComplete:  true,
		TurnComplete:        true,
	}}
	evs := translate(m, now)
	want := []Kind{Audio, Audio, OutputText, GenerationComplete, TurnComplete}
	got := kinds(evs)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if evs[0].Rate != 24000 || evs[1].Rate != 24000 {
		t.Fatalf("rates = %d, %d; want 24000 (the second from the default)", evs[0].Rate, evs[1].Rate)
	}
	if !evs[0].At.Equal(now) {
		t.Fatalf("At = %v, want %v", evs[0].At, now)
	}
}

func TestTranslateControlMessages(t *testing.T) {
	m := &genai.LiveServerMessage{
		ToolCall:                &genai.LiveServerToolCall{FunctionCalls: []*genai.FunctionCall{{ID: "c1", Name: "add_node"}}},
		GoAway:                  &genai.LiveServerGoAway{TimeLeft: 30 * time.Second},
		SessionResumptionUpdate: &genai.LiveServerSessionResumptionUpdate{NewHandle: "h1", Resumable: true},
		VoiceActivity:           &genai.VoiceActivity{VoiceActivityType: genai.VoiceActivityTypeActivityEnd},
	}
	evs := translate(m, time.Now())
	got := map[Kind]Event{}
	for _, e := range evs {
		got[e.Kind] = e
	}
	if got[ToolCall].Calls[0].ID != "c1" {
		t.Fatalf("tool call = %+v", got[ToolCall])
	}
	if got[GoAway].TimeLeft != 30*time.Second {
		t.Fatalf("go away = %+v", got[GoAway])
	}
	if got[Resumable].Handle != "h1" {
		t.Fatalf("resumable = %+v", got[Resumable])
	}
	if _, ok := got[VoiceEnd]; !ok {
		t.Fatalf("no VoiceEnd in %v", kinds(evs))
	}

	// A handle the server marks not resumable is not worth keeping.
	m = &genai.LiveServerMessage{SessionResumptionUpdate: &genai.LiveServerSessionResumptionUpdate{NewHandle: "h2"}}
	if evs := translate(m, time.Now()); len(evs) != 0 {
		t.Fatalf("non-resumable update gave %v", kinds(evs))
	}
}

func TestRateOf(t *testing.T) {
	for mime, want := range map[string]int{
		"audio/pcm;rate=16000":  16000,
		"audio/pcm; rate=48000": 48000,
		"audio/pcm":             24000,
		"audio/pcm;rate=bad":    24000,
	} {
		if got := rateOf(mime); got != want {
			t.Errorf("rateOf(%q) = %d, want %d", mime, got, want)
		}
	}
}

func TestConnectConfigVoiceDetection(t *testing.T) {
	if c := connectConfig(Config{}); c.RealtimeInputConfig != nil {
		t.Fatalf("no settings sent a RealtimeInputConfig: %+v", c.RealtimeInputConfig)
	}
	c := connectConfig(Config{StartOfSpeech: "low", PrefixMS: 300, EndOfSpeech: "high"})
	vad := c.RealtimeInputConfig.AutomaticActivityDetection
	if vad.StartOfSpeechSensitivity != genai.StartSensitivityLow || vad.PrefixPaddingMs == nil || *vad.PrefixPaddingMs != 300 ||
		vad.EndOfSpeechSensitivity != genai.EndSensitivityHigh || vad.SilenceDurationMs != nil {
		t.Fatalf("vad = %+v", vad)
	}
}

func TestConnectConfigMediaResolution(t *testing.T) {
	if c := connectConfig(Config{}); c.MediaResolution != "" {
		t.Fatalf("no setting sent %q", c.MediaResolution)
	}
	if c := connectConfig(Config{MediaResolution: "high"}); c.MediaResolution != genai.MediaResolutionHigh {
		t.Fatalf("high sent %q", c.MediaResolution)
	}
}

func TestConnectConfigVideo(t *testing.T) {
	c := connectConfig(Config{Video: true, EndOfSpeech: "high"})
	ric := c.RealtimeInputConfig
	if ric.TurnCoverage != genai.TurnCoverageTurnIncludesAudioActivityAndAllVideo || ric.AutomaticActivityDetection == nil {
		t.Fatalf("realtime input config = %+v", ric)
	}
}
