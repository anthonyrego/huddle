package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// openTest connects to HUDDLE_TEST_DATABASE_URL (make test-db starts one)
// and migrates it, or skips. Each test uses its own meeting id, so runs
// don't collide.
func openTest(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HUDDLE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set HUDDLE_TEST_DATABASE_URL to run against Postgres")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func meetingID(t *testing.T) string {
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestMigrationFilesAreNumbered(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || files[0].version != 1 {
		t.Fatalf("want migrations starting at 1, got %+v", files)
	}
}

func TestMigrateTwiceAppliesNothing(t *testing.T) {
	s := openTest(t)
	applied, err := s.Migrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v", applied)
	}
}

func TestCommitAndRace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := meetingID(t)
	if _, err := s.EnsureMeeting(ctx, id); err != nil {
		t.Fatal(err)
	}
	doc := json.RawMessage(`{"nodes":[]}`)
	ops := []json.RawMessage{json.RawMessage(`{"op":"add_node","id":"a"}`), json.RawMessage(`{"op":"add_node","id":"b"}`)}
	v, err := s.Commit(ctx, Version{Meeting: id, Requester: "sam", Command: "c1", TMs: 1200, Doc: doc, Ops: ops}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Seq != 1 || v.Parent != 0 {
		t.Fatalf("first version is %d (parent %d)", v.Seq, v.Parent)
	}
	// A second writer that also read head 0 loses.
	if _, err := s.Commit(ctx, Version{Meeting: id, TMs: 1300, Doc: doc}, 0); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("racing commit: %v, want ErrHeadMoved", err)
	}
	head, err := s.Head(ctx, id)
	if err != nil || head != 1 {
		t.Fatalf("head %d, %v", head, err)
	}
	got, err := s.Version(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requester != "sam" || got.TMs != 1200 {
		t.Fatalf("read back %+v", got)
	}
	gotOps, err := s.Ops(ctx, id, 1)
	if err != nil || len(gotOps) != 2 {
		t.Fatalf("ops %v, %v", gotOps, err)
	}
	if _, err := s.Version(ctx, id, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
}

func TestSegmentsAndAgentState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := meetingID(t)
	if _, err := s.EnsureMeeting(ctx, id); err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"first", "second", "third"} {
		if err := s.AddSegment(ctx, Segment{Meeting: id, Identity: "p1", Speaker: "Sam", StartMs: int64(i * 1000), EndMs: int64(i*1000 + 900), Text: txt}); err != nil {
			t.Fatal(err)
		}
	}
	segs, err := s.Segments(ctx, id, 500, 2500)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Text != "second" {
		t.Fatalf("segments %+v", segs)
	}
	if a, err := s.AgentState(ctx, id); err != nil || a.ResumeHandle != "" {
		t.Fatalf("empty agent state %+v, %v", a, err)
	}
	if err := s.SetAgentState(ctx, id, AgentState{ResumeHandle: "h1", Summary: "so far"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.AgentState(ctx, id); a.ResumeHandle != "h1" || a.Summary != "so far" {
		t.Fatalf("agent state %+v", a)
	}
}
