package rtctoken

import (
	"strings"
	"testing"
	"time"
)

func TestMintVerify(t *testing.T) {
	m := Minter{Secret: []byte("dev secret"), TTL: time.Minute}
	g := Grant{Room: "demo", Identity: "whiteboard", Name: "Whiteboard", Agent: true}
	tok, err := m.Mint(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Verify(tok, time.Now())
	if err != nil || got != g {
		t.Fatalf("verify: %+v, %v", got, err)
	}
	if _, err := m.Verify(tok, time.Now().Add(2*time.Minute)); err != ErrExpired {
		t.Errorf("after the TTL: %v", err)
	}
	if _, err := (Minter{Secret: []byte("another")}).Verify(tok, time.Now()); err != ErrBad {
		t.Errorf("another secret: %v", err)
	}
	p, sig, _ := strings.Cut(tok, ".")
	forged := b64.EncodeToString([]byte(`{"room":"demo","id":"someone","exp":9999999999}`)) + "." + sig
	if _, err := m.Verify(forged, time.Now()); err != ErrBad {
		t.Errorf("claims changed: %v", err)
	}
	for _, bad := range []string{"", "x", p, p + ".", "." + sig} {
		if _, err := m.Verify(bad, time.Now()); err != ErrBad {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := (Minter{}).Mint(g); err != ErrNoSecret {
		t.Errorf("no secret: %v", err)
	}
	if _, err := m.Mint(Grant{Room: "demo"}); err == nil {
		t.Error("a token with no identity")
	}
}
