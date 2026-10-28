package board

import (
	"path/filepath"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	ops, err := ReadOps(filepath.Join("..", "..", "testdata", "workflows", "orders.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalSnapshot(ops)
	if err != nil {
		t.Fatal(err)
	}
	s, err := UnmarshalSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Replay(ops)
	got, errs := s.Doc()
	if len(errs) > 0 {
		t.Fatalf("replay errors %v", errs)
	}
	if want.Digest() != got.Digest() {
		t.Fatalf("snapshot rebuilt a different board:\n%s\nvs\n%s", got.Digest(), want.Digest())
	}
}
