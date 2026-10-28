package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/board/layout"
	"github.com/anthonyrego/huddle/internal/board/measure"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Store is what the whiteboard needs from the server's store.
type Store interface {
	EnsureMeeting(ctx context.Context, id string) (store.Meeting, error)
	Head(ctx context.Context, meeting string) (int64, error)
	Version(ctx context.Context, meeting string, seq int64) (store.Version, error)
	Ops(ctx context.Context, meeting string, seq int64) ([]json.RawMessage, error)
	Commit(ctx context.Context, v store.Version, expectHead int64) (store.Version, error)
	AddSegment(ctx context.Context, g store.Segment) error
	Segments(ctx context.Context, meeting string, fromMs, toMs int64) ([]store.Segment, error)
	AgentState(ctx context.Context, meeting string) (store.AgentState, error)
	SetAgentState(ctx context.Context, meeting string, a store.AgentState) error
	Voice(ctx context.Context, meeting string) (string, error)
}

// Hub is how the whiteboard tells the room's apps.
type Hub interface {
	Send(room, kind string, body any) error
	Count(room string) int
}

// boardState is the board as of the head version, kept by the agent's
// loop.
type boardState struct {
	room  string
	st    Store
	hub   Hub
	head  int64
	doc   *board.Doc
	place *layout.Placement
	sizer measure.Sizer
}

// load reads the head version.
func (b *boardState) load(ctx context.Context) error {
	head, err := b.st.Head(ctx, b.room)
	if err != nil {
		return err
	}
	b.head, b.doc, b.place = head, board.New(), nil
	if head == 0 {
		return nil
	}
	v, err := b.st.Version(ctx, b.room, head)
	if err != nil {
		return err
	}
	doc, err := snapshotDoc(v.Doc)
	if err != nil {
		return err
	}
	b.doc = doc
	if len(v.Layout) > 0 {
		var pl layout.Placement
		if json.Unmarshal(v.Layout, &pl) == nil {
			b.place = &pl
		}
	}
	return nil
}

func snapshotDoc(raw json.RawMessage) (*board.Doc, error) {
	snap, err := board.UnmarshalSnapshot(raw)
	if err != nil {
		return nil, err
	}
	doc, _ := snap.Doc()
	return doc, nil
}

// clone is a copy of the board to change.
func clone(d *board.Doc) *board.Doc {
	c, _ := board.Replay(d.Ops())
	return c
}

// change is one new version: the board it ends at, the ops that made it,
// and who asked.
type change struct {
	doc       *board.Doc
	ops       []board.Op // what the model called (or the undo), for the reveal
	requester string
	command   string
	tMs       int64
	restored  int64 // the version restored, for undo and revert_to (-1: none)
	redo      func(*board.Doc) (*board.Doc, []board.Op)
}

// commit stores a change as the next version and tells the room. If
// another writer got there first, it re-reads the head and makes the
// change again on top of it.
func (b *boardState) commit(ctx context.Context, c change) (store.Version, error) {
	for attempt := 0; ; attempt++ {
		pl := layout.Board(c.doc, &b.sizer, b.place)
		snap, err := board.MarshalSnapshot(c.doc.Ops())
		if err != nil {
			return store.Version{}, err
		}
		plJSON, err := json.Marshal(pl)
		if err != nil {
			return store.Version{}, err
		}
		raw := make([]json.RawMessage, len(c.ops))
		for i, op := range c.ops {
			raw[i], _ = json.Marshal(op)
		}
		v := store.Version{Meeting: b.room, Requester: c.requester, Command: c.command, TMs: c.tMs, Doc: snap, Layout: plJSON, Ops: raw}
		if c.restored > 0 {
			v.RestoredFrom = c.restored
		}
		v, err = b.st.Commit(ctx, v, b.head)
		if errors.Is(err, store.ErrHeadMoved) && attempt < 5 && c.redo != nil {
			if err := b.load(ctx); err != nil {
				return store.Version{}, err
			}
			c.doc, c.ops = c.redo(b.doc)
			time.Sleep(time.Duration(attempt*20) * time.Millisecond)
			continue
		}
		if err != nil {
			return store.Version{}, err
		}
		b.head, b.doc, b.place = v.Seq, c.doc, pl
		b.hub.Send(b.room, wire.KindBoardVersion, wire.BoardVersion{
			Room: b.room, Seq: v.Seq, Parent: v.Parent, Requester: v.Requester, Command: v.Command,
			TMs: v.TMs, Doc: snap, Layout: plJSON,
		})
		b.hub.Send(b.room, wire.KindBoardOps, wire.BoardOps{Room: b.room, Seq: v.Seq, Ops: raw})
		return v, nil
	}
}

// applyOps makes a change from ops on top of doc: the ones that apply.
func applyOps(doc *board.Doc, ops []board.Op) (*board.Doc, []board.Op) {
	d := clone(doc)
	var kept []board.Op
	for _, op := range ops {
		if d.Apply(op) == nil {
			kept = append(kept, op)
		}
	}
	return d, kept
}

// restoredFrom is the version a version restored, and whether it was a
// restore at all (an undo to the empty board restores version 0).
func (b *boardState) restoredFrom(ctx context.Context, seq int64) (int64, bool, error) {
	v, err := b.st.Version(ctx, b.room, seq)
	if err != nil {
		return 0, false, err
	}
	if v.RestoredFrom > 0 {
		return v.RestoredFrom, true, nil
	}
	ops, err := b.st.Ops(ctx, b.room, seq)
	if err != nil {
		return 0, false, err
	}
	for _, raw := range ops {
		if op, err := board.ParseOp(raw); err == nil && op.Versioned() {
			return op.Version, true, nil
		}
	}
	return 0, false, nil
}

// undoTarget is the version an undo goes back to: the one before the
// change the board shows. Undoing an undo steps back further.
func (b *boardState) undoTarget(ctx context.Context) (int64, error) {
	seq := b.head
	for range 1000 {
		if seq <= 0 {
			return -1, nil
		}
		from, ok, err := b.restoredFrom(ctx, seq)
		if err != nil {
			return 0, err
		}
		if !ok {
			return seq - 1, nil
		}
		seq = from
	}
	return 0, errors.New("undo: too many restores in a row")
}

// versionDoc is the board at a version (0: empty).
func (b *boardState) versionDoc(ctx context.Context, seq int64) (*board.Doc, error) {
	if seq == 0 {
		return board.New(), nil
	}
	if seq < 0 || seq > b.head {
		return nil, fmt.Errorf("there is no version %d (the board is at version %d)", seq, b.head)
	}
	v, err := b.st.Version(ctx, b.room, seq)
	if err != nil {
		return nil, err
	}
	return snapshotDoc(v.Doc)
}

// restore makes a new version that is version target again.
func (b *boardState) restore(ctx context.Context, op board.Op, target int64, requester, command string, tMs int64) (store.Version, error) {
	doc, err := b.versionDoc(ctx, target)
	if err != nil {
		return store.Version{}, err
	}
	op.Version = target
	c := change{doc: doc, ops: []board.Op{op}, requester: requester, command: command, tMs: tMs, restored: target}
	c.redo = func(*board.Doc) (*board.Doc, []board.Op) { return doc, []board.Op{op} }
	return b.commit(ctx, c)
}
