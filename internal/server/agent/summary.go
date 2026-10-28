package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/anthonyrego/huddle/internal/server/store"
)

// The rolling summary: every few minutes of talk, a small text model folds
// the new transcript into a short summary of the meeting. Each turn
// carries it, so the brain keeps the thread of a long meeting after its
// own context has been compressed, or when a connection starts fresh.

const (
	summarizeChars = 2500            // new transcript that brings a new summary
	summarizeEvery = 5 * time.Minute // or this long with anything new
)

func (a *Agent) maybeSummarize() {
	if a.summing {
		return
	}
	var fresh []store.Segment
	n := 0
	for _, s := range a.segs {
		if s.EndMs > a.summedTo {
			fresh = append(fresh, s)
			n += len(s.Text)
		}
	}
	if len(fresh) == 0 {
		return
	}
	age := time.Duration(a.now()-a.summedTo) * time.Millisecond
	if n < summarizeChars && age < summarizeEvery {
		return
	}
	a.summing = true
	prev, to := a.summary, fresh[len(fresh)-1].EndMs
	var b strings.Builder
	for _, s := range fresh {
		fmt.Fprintf(&b, "[%s] %s: %s\n", clockText(s.StartMs), s.Speaker, s.Text)
	}
	digest := a.b.doc.Digest()
	go func() {
		s, tokens, err := summarize(a.cfg.APIKey, a.cfg.SummaryModel, prev, b.String(), digest)
		a.trace(Event{Kind: EvUsage, N: tokens, Detail: "summary"})
		a.post(func() {
			a.summing = false
			if err != nil {
				a.log.Warn("agent: summary", "err", err)
				a.trace(Event{Kind: EvError, Detail: "summary: " + err.Error()})
				return
			}
			a.summary, a.summedTo = s, to
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			a.cfg.Store.SetAgentState(ctx, a.cfg.Room, store.AgentState{ResumeHandle: a.brain.handle, Summary: s})
			cancel()
			a.log.Info("agent: summary", "chars", len(s))
		})
	}()
}

// summarize returns the new summary and the tokens it took.
func summarize(key, model, prev, transcript, digest string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return "", 0, err
	}
	prompt := "You keep the running summary of a meeting for its AI whiteboard. Fold the new part of the transcript into the summary: " +
		"who is there, the topics in order, decisions, open questions, and anything someone asked the whiteboard to do. " +
		"At most 180 words, plain prose, no preamble.\n\nSummary so far:\n" + orNone(prev) +
		"\n\nNew transcript:\n" + transcript + "\nThe board now:\n" + digest
	res, err := c.Models.GenerateContent(ctx, model, genai.Text(prompt), nil)
	if err != nil {
		return "", 0, err
	}
	tokens := 0
	if u := res.UsageMetadata; u != nil {
		tokens = int(u.TotalTokenCount)
	}
	s := strings.TrimSpace(res.Text())
	if s == "" {
		return "", tokens, fmt.Errorf("an empty summary")
	}
	return s, tokens, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}
