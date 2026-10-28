package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/server/store"
)

func init() {
	register(&command{name: "put-board", summary: "dev: store a fixture's ops as the room's next board version (apps see it when they join)", run: runPutBoard})
}

func runPutBoard(args []string) error {
	fs := flag.NewFlagSet("put-board", flag.ContinueOnError)
	room := fs.String("room", "", "the room (required)")
	fixture := fs.String("fixture", "", "ops file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := os.Getenv("DATABASE_URL")
	if *room == "" || *fixture == "" || url == "" {
		return errors.New("-room, -fixture and DATABASE_URL are required")
	}
	ops, err := board.ReadOps(*fixture)
	if err != nil {
		return err
	}
	if _, errs := board.Replay(ops); len(errs) > 0 {
		return fmt.Errorf("the fixture doesn't replay: %v", errs[0])
	}
	doc, err := board.MarshalSnapshot(ops)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.EnsureMeeting(ctx, *room); err != nil {
		return err
	}
	head, err := st.Head(ctx, *room)
	if err != nil {
		return err
	}
	raw := make([]json.RawMessage, len(ops))
	for i, op := range ops {
		raw[i], _ = json.Marshal(op)
	}
	v, err := st.Commit(ctx, store.Version{Meeting: *room, Requester: "put-board", Doc: doc, Ops: raw}, head)
	if err != nil {
		return err
	}
	fmt.Printf("%s: version %d (%d ops)\n", *room, v.Seq, len(ops))
	return nil
}
