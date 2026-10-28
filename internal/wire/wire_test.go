package wire

import (
	"encoding/json"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	b, err := Encode(KindCommand, Command{Room: "r", Text: "draw the flow"})
	if err != nil {
		t.Fatal(err)
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindCommand {
		t.Fatalf("kind %q", e.Kind)
	}
	var c Command
	if err := e.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Text != "draw the flow" || c.Room != "r" {
		t.Fatalf("got %+v", c)
	}
}
