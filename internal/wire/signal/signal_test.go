package signal

import (
	"encoding/json"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	mid, idx := "0", uint16(0)
	msgs := []struct {
		kind string
		body any
		into any
	}{
		{KindJoin, Join{Protocol: Protocol, Token: "t", Subscribe: SubscribeNone}, &Join{}},
		{KindPublish, Publish{CID: "cam", Kind: Video, Source: Camera, Layers: []Layer{{"f", 1280, 720, 1_700_000}, {"h", 640, 360, 450_000}}}, &Publish{}},
		{KindICE, ICE{PC: Sub, Candidate: "candidate:1 1 udp 1 127.0.0.1 9 typ host", SDPMid: &mid, SDPMLineIndex: &idx}, &ICE{}},
		{KindSettings, Settings{SID: "TR_1", W: 342, H: 192, Enabled: true}, &Settings{}},
	}
	for _, m := range msgs {
		b, err := Marshal(m.kind, m.body)
		if err != nil {
			t.Fatal(err)
		}
		var e Envelope
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != m.kind {
			t.Fatalf("kind %q, want %q", e.Kind, m.kind)
		}
		if err := e.Decode(m.into); err != nil {
			t.Fatal(err)
		}
		again, _ := json.Marshal(m.into)
		want, _ := json.Marshal(m.body)
		if string(again) != string(want) {
			t.Errorf("%s: %s, want %s", m.kind, again, want)
		}
	}
	// A message with no body.
	b, _ := Marshal(KindLeave, nil)
	if string(b) != `{"kind":"leave"}` {
		t.Errorf("leave: %s", b)
	}
}
