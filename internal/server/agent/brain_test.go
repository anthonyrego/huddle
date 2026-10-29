package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/live"
)

// TestTurnCompleteOwed: a turn the model hung on (ended by no reply) owes
// its last turn-complete, which comes once the next turn is sent; it
// doesn't end that turn. (Step 4's integration: a question's answer never
// got its turn-complete, and the command after it closed 25 ms in with
// nothing drawn.)
func TestTurnCompleteOwed(t *testing.T) {
	var ends []string
	a := New(Config{Room: "r", Hub: &memHub{}, Trace: func(e Event) {
		if e.Kind == EvTurnEnd {
			ends = append(ends, e.Command+": "+e.Detail)
		}
	}})
	ctx := context.Background()
	done := live.Event{Kind: live.TurnComplete}
	audio := live.Event{Kind: live.Audio, Data: make([]byte, 960), Rate: 24000}
	start := func(id string) { a.turn = &turn{cmd: gate.Command{ID: id}, started: time.Now(), lastEv: time.Now()} }
	hang := func() {
		a.turn.lastEv = time.Now().Add(-21 * time.Second)
		a.turn.check(ctx, a)
	}

	// A question that spoke, then went quiet before its last turn-complete.
	start("c3")
	a.onBrain(ctx, audio, true)
	hang()
	// The next command: the turn before's turn-complete first, then its own.
	start("c4")
	a.onBrain(ctx, done, true)
	if a.turn == nil {
		t.Fatalf("the turn before's turn-complete ended c4: %v", ends)
	}
	a.onBrain(ctx, audio, true)
	a.onBrain(ctx, done, true)
	if a.turn != nil {
		t.Fatal("c4's own turn-complete didn't end it")
	}

	// Nothing owed: a turn-complete ends a turn, even one that sent
	// nothing of its own.
	start("c5")
	a.onBrain(ctx, done, true)

	// Owed, and nothing of the next turn's own comes: it ends by no reply,
	// as before.
	start("c6")
	hang()
	start("c7")
	a.onBrain(ctx, done, true)
	hang()

	// A turn-complete between turns is the one owed.
	start("c8")
	hang()
	a.onBrain(ctx, done, true)
	start("c9")
	a.onBrain(ctx, done, true)

	want := []string{"c3: no reply", "c4: turn-complete", "c5: turn-complete", "c6: no reply", "c7: no reply", "c8: no reply", "c9: turn-complete"}
	if !slices.Equal(ends, want) {
		t.Fatalf("turns ended %q, want %q", ends, want)
	}
}
