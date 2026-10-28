package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/wire"
)

// memStore is the store in memory.
type memStore struct {
	mu       sync.Mutex
	versions []store.Version // seq i+1 at i
	segs     []store.Segment
	state    store.AgentState
	// steal makes the next commit lose the race to another writer.
	steal bool
}

func (m *memStore) EnsureMeeting(ctx context.Context, id string) (store.Meeting, error) {
	return store.Meeting{ID: id, StartedAt: time.Unix(1000, 0)}, nil
}
func (m *memStore) Head(ctx context.Context, meeting string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.versions)), nil
}
func (m *memStore) Version(ctx context.Context, meeting string, seq int64) (store.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq < 1 || int(seq) > len(m.versions) {
		return store.Version{}, store.ErrNotFound
	}
	return m.versions[seq-1], nil
}
func (m *memStore) Ops(ctx context.Context, meeting string, seq int64) ([]json.RawMessage, error) {
	v, err := m.Version(ctx, meeting, seq)
	return v.Ops, err
}
func (m *memStore) Commit(ctx context.Context, v store.Version, expectHead int64) (store.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.steal {
		m.steal = false
		doc, _ := board.Replay([]board.Op{{Name: board.AddNode, ID: "intruder", Kind: board.Idea, Label: "Put there by put-board"}})
		snap, _ := board.MarshalSnapshot(doc.Ops())
		m.versions = append(m.versions, store.Version{Meeting: v.Meeting, Seq: int64(len(m.versions)) + 1, Parent: int64(len(m.versions)), Doc: snap})
	}
	if expectHead != int64(len(m.versions)) {
		return store.Version{}, store.ErrHeadMoved
	}
	v.Parent, v.Seq = expectHead, expectHead+1
	m.versions = append(m.versions, v)
	return v, nil
}
func (m *memStore) AddSegment(ctx context.Context, g store.Segment) error {
	m.mu.Lock()
	m.segs = append(m.segs, g)
	m.mu.Unlock()
	return nil
}
func (m *memStore) Segments(ctx context.Context, meeting string, from, to int64) ([]store.Segment, error) {
	var out []store.Segment
	for _, s := range m.segs {
		if s.StartMs >= from && s.StartMs < to {
			out = append(out, s)
		}
	}
	return out, nil
}
func (m *memStore) AgentState(ctx context.Context, meeting string) (store.AgentState, error) {
	return m.state, nil
}
func (m *memStore) SetAgentState(ctx context.Context, meeting string, a store.AgentState) error {
	m.state = a
	return nil
}
func (m *memStore) Voice(ctx context.Context, meeting string) (string, error) { return "", nil }

// memHub keeps what was sent.
type memHub struct {
	mu   sync.Mutex
	sent []string
}

func (h *memHub) Send(room, kind string, body any) error {
	h.mu.Lock()
	h.sent = append(h.sent, kind)
	h.mu.Unlock()
	return nil
}
func (h *memHub) Count(room string) int { return 1 }

func draw(t *testing.T, b *boardState, ops ...board.Op) {
	t.Helper()
	doc, kept := applyOps(b.doc, ops)
	if len(kept) != len(ops) {
		t.Fatalf("ops didn't apply: %v", ops)
	}
	c := change{doc: doc, ops: ops, restored: -1, redo: func(h *board.Doc) (*board.Doc, []board.Op) { return applyOps(h, ops) }}
	if _, err := b.commit(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func ids(d *board.Doc) string {
	var out []string
	for _, n := range d.Nodes() {
		out = append(out, n.ID)
	}
	return strings.Join(out, ",")
}

func TestUndoStepsBackAndRevert(t *testing.T) {
	ctx := context.Background()
	st, hub := &memStore{}, &memHub{}
	b := &boardState{room: "r", st: st, hub: hub}
	if err := b.load(ctx); err != nil {
		t.Fatal(err)
	}
	add := func(id string) board.Op { return board.Op{Name: board.AddNode, ID: id, Kind: board.Idea, Label: id} }
	draw(t, b, add("a"))
	draw(t, b, add("b"))
	draw(t, b, add("c"))
	if ids(b.doc) != "a,b,c" || b.head != 3 || b.place == nil || len(b.place.Boxes) != 3 {
		t.Fatalf("board %s at %d", ids(b.doc), b.head)
	}
	undo := func() int64 {
		target, err := b.undoTarget(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if target < 0 {
			return target
		}
		if _, err := b.restore(ctx, board.Op{Name: board.Undo}, target, "Sam", "c9", 0); err != nil {
			t.Fatal(err)
		}
		return target
	}
	// Each undo steps back one more change.
	for _, want := range []string{"a,b", "a", ""} {
		undo()
		if got := ids(b.doc); got != want {
			t.Fatalf("after undo: %q, want %q", got, want)
		}
	}
	if target := undo(); target != -1 {
		t.Fatalf("undo past the start went to %d", target)
	}
	// Revert to version 3 brings everything back; undo then goes to 2.
	v, err := b.restore(ctx, board.Op{Name: board.RevertTo}, 3, "Sam", "c10", 0)
	if err != nil || ids(b.doc) != "a,b,c" || v.RestoredFrom != 3 {
		t.Fatalf("revert: %v, %s, %+v", err, ids(b.doc), v)
	}
	undo()
	if got := ids(b.doc); got != "a,b" {
		t.Fatalf("undo after revert: %q", got)
	}
	if _, err := b.versionDoc(ctx, 99); err == nil {
		t.Fatal("reverting to a version that doesn't exist went through")
	}
	// Every version went out with its ops.
	n := 0
	for _, k := range hub.sent {
		if k == wire.KindBoardVersion {
			n++
		}
	}
	if int64(n) != b.head {
		t.Fatalf("%d versions sent for %d", n, b.head)
	}
}

func TestCommitRedoesWhenTheHeadMoved(t *testing.T) {
	ctx := context.Background()
	st := &memStore{}
	b := &boardState{room: "r", st: st, hub: &memHub{}}
	b.load(ctx)
	draw(t, b, board.Op{Name: board.AddNode, ID: "a", Kind: board.Idea, Label: "A"})
	st.steal = true
	draw(t, b, board.Op{Name: board.AddNode, ID: "b", Kind: board.Idea, Label: "B", After: "a"})
	// Another writer replaced the board; the change was made again on it,
	// keeping what still applies.
	if b.head != 3 || ids(b.doc) != "intruder" {
		t.Fatalf("head %d, board %s", b.head, ids(b.doc))
	}
}

func TestTurnTextAndRecent(t *testing.T) {
	doc, _ := board.Replay([]board.Op{{Name: board.AddNode, ID: "a", Kind: board.Step, Label: "Build"}})
	segs := []store.Segment{{Speaker: "Kim", StartMs: 61000, Text: "Builds take ages."}, {Speaker: "Sam", StartMs: 65000, Text: "Whiteboard, add test."}}
	c := gate.Command{Speaker: "Sam", Text: "add test.", Source: gate.Typed, EndMs: 66000}
	s := turnText(c, 4, segs, "They are planning a release.", doc, false, 66000)
	for _, want := range []string{`Command from Sam at 1:06 (typed): "add test."`, "version 4", "[1:01] Kim: Builds take ages.", "The meeting so far: They are planning", `step a "Build"`} {
		if !strings.Contains(s, want) {
			t.Errorf("turn text lacks %q:\n%s", want, s)
		}
	}
	if a := turnText(c, 4, nil, "", doc, true, 66000); !strings.Contains(a, "Auto update at 1:06") {
		t.Errorf("auto turn text:\n%s", a)
	}
	if got := recentText(segs, 40); len(got) != 1 || got[0].Speaker != "Sam" {
		t.Fatalf("recent = %+v", got)
	}
}

func TestVoiceAllowed(t *testing.T) {
	a := &Agent{}
	for _, c := range []struct {
		policy             string
		spoke, muted, auto bool
		want               bool
	}{
		{VoiceAnswers, false, false, false, false},
		{VoiceAnswers, true, false, false, true},
		{VoiceAnswers, true, true, false, false},
		{VoiceAnswers, true, false, true, false},
		{VoiceMuted, true, false, false, false},
		{VoiceNarrate, false, false, false, true},
		{VoiceNarrate, false, false, true, false},
	} {
		a.policy = c.policy
		if got := a.voiceAllowed(&turn{spoke: c.spoke, muted: c.muted, auto: c.auto}); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func TestVoiceOutResamplesAndPaces(t *testing.T) {
	enc := &countEnc{}
	var on []bool
	v := newVoiceOut(enc, func(b bool) { on = append(on, b) })
	pcm := make([]byte, 24000*2/10) // 100 ms at 24 kHz
	for i := range pcm {
		pcm[i] = byte(i)
	}
	v.push(pcm, 24000)
	if len(v.buf) != 4800 {
		t.Fatalf("100 ms at 48 kHz is %d samples", len(v.buf))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	var tss []uint32
	v.run(ctx, func(p []byte, ts uint32) error { tss = append(tss, ts); return nil })
	// The 100 ms is five frames; 400 ms after the last, a silent one keeps
	// the track going, its timestamp following the clock.
	if len(tss) != 6 || len(on) != 2 || !on[0] || on[1] {
		t.Fatalf("frames %d, speaking edges %v", len(tss), on)
	}
	if gap := tss[5] - tss[4]; gap != keepAlive*frameLen {
		t.Fatalf("the silent frame came %d samples after the last, want %d", gap, keepAlive*frameLen)
	}
	v.push(pcm, 24000)
	v.cut()
	if len(v.buf) != 0 {
		t.Fatal("cut kept audio")
	}
}

// TestVoiceOutTimestampsFollowTheClock: within an answer the RTP timestamp
// steps a frame at a time; across the quiet before the next answer it
// steps by the time that went by, as a mic's does.
func TestVoiceOutTimestampsFollowTheClock(t *testing.T) {
	v := newVoiceOut(&countEnc{}, nil)
	pcm := make([]byte, 24000*2/25) // 40 ms at 24 kHz: two frames
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var ts []uint32
	var mu sync.Mutex
	go func() {
		v.push(pcm, 24000)
		time.Sleep(250 * time.Millisecond)
		v.push(pcm, 24000)
	}()
	v.run(ctx, func(p []byte, at uint32) error { mu.Lock(); ts = append(ts, at); mu.Unlock(); return nil })
	mu.Lock()
	defer mu.Unlock()
	if len(ts) != 4 {
		t.Fatalf("%d packets, want 4", len(ts))
	}
	if ts[1]-ts[0] != frameLen || ts[3]-ts[2] != frameLen {
		t.Errorf("steps within an answer: %d and %d, want %d", ts[1]-ts[0], ts[3]-ts[2], frameLen)
	}
	if gap := ts[2] - ts[1]; gap < 8*frameLen || gap > 14*frameLen {
		t.Errorf("step across ~210 ms of quiet: %d samples (%d frames)", gap, gap/frameLen)
	}
}

type countEnc struct{}

func (countEnc) Encode(pcm []int16) ([]byte, error) { return []byte{1}, nil }
func (countEnc) Close() error                       { return nil }

func TestDefaultEncoder(t *testing.T) {
	e, err := NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	pkt, err := e.Encode(make([]int16, frameLen))
	if err != nil || len(pkt) == 0 {
		t.Fatalf("encode: %d bytes, %v", len(pkt), err)
	}
}
