package agent

import (
	"fmt"
	"strings"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/server/store"
)

// Voice policies: when the whiteboard's voice is heard in the room.
const (
	VoiceAnswers = "answers" // silent while drawing, speaks only when asked a question
	VoiceMuted   = "muted"   // never heard
	VoiceNarrate = "narrate" // says what it drew, and answers questions
)

// system is the brain's standing instruction. The tool descriptions are
// the rest of its manual.
func system(policy string) string {
	voice := `Voice: draw in silence. Only when the command is a question to you that needs an answer in words, or asks you to explain or say something, call speak first and then answer aloud in one or two short sentences. Never speak otherwise: no greetings, no "done", no reading the board back.`
	if policy == VoiceNarrate {
		voice = `Voice: after you draw, say in a few words what you changed. When the command is a question to you, call speak first and answer aloud in one or two short sentences.`
	}
	return `You are the whiteboard in a meeting of several people. You don't hear the meeting directly. Each turn you get is one command that someone addressed to you (by saying "whiteboard", by typing, or with a key), with who asked, what was said in the meeting since your last turn, a summary of the meeting so far, and the board as it is now.

Do what the command asks with the board tools, making all of a command's calls at once. Keep box labels short (2 to 5 words) and in the speakers' own words. Reuse a box's id to change it. When you put a step between two others, remove the arrow that joined them. Use groups for categories, sides of a comparison and phases; arrows (after, connect) for order and cause; the notes panel for decisions, action items with owners, and open questions. "Draw what we just discussed" means: turn the recent transcript into a diagram. Sort with order when asked for an order (alphabetically, by time, or one given). Undo and revert_to take the board back. When asked to keep diagramming or to take notes as the meeting goes, call auto with enabled true; you then get update turns with what was said, and should add what's new and worth keeping, or nothing. When a command isn't something you can do on the board and isn't a question, do nothing.

` + voice
}

// clockText is a meeting time as m:ss.
func clockText(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// turnText is what the brain gets for one command.
func turnText(c gate.Command, version int64, recent []store.Segment, summary string, doc *board.Doc, auto bool, nowMs int64) string {
	var b strings.Builder
	switch {
	case auto:
		fmt.Fprintf(&b, "Auto update at %s. Add to the board and the notes panel anything new and worth keeping from what was said below; if nothing is, call nothing. Draw in silence.\n", clockText(nowMs))
	default:
		how := "said aloud"
		switch c.Source {
		case gate.Typed:
			how = "typed"
		case gate.Key:
			how = "said holding the whiteboard key"
		}
		fmt.Fprintf(&b, "Command from %s at %s (%s): %q\n", c.Speaker, clockText(c.EndMs), how, c.Text)
	}
	fmt.Fprintf(&b, "The board is at version %d.\n", version)
	if len(recent) > 0 {
		b.WriteString("\nWhat was said since your last turn:\n")
		for _, s := range recent {
			fmt.Fprintf(&b, "[%s] %s: %s\n", clockText(s.StartMs), s.Speaker, s.Text)
		}
	}
	if summary != "" {
		fmt.Fprintf(&b, "\nThe meeting so far: %s\n", summary)
	}
	fmt.Fprintf(&b, "\nThe board now:\n%s\n", doc.Digest())
	return b.String()
}

// recentText caps the transcript sent with a turn: the newest segments
// that fit in max bytes.
func recentText(segs []store.Segment, max int) []store.Segment {
	n := 0
	for i := len(segs) - 1; i >= 0; i-- {
		n += len(segs[i].Text) + len(segs[i].Speaker) + 12
		if n > max {
			return segs[i+1:]
		}
	}
	return segs
}
