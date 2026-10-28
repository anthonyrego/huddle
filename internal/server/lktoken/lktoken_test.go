package lktoken

import (
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
)

func TestMintVerifies(t *testing.T) {
	m := Minter{Key: "devkey", Secret: "a-secret-of-at-least-32-characters!", TTL: time.Minute}
	tok, err := m.Mint(Grant{Room: "standup", Identity: "sam-1a2b", Name: "Sam"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.ParseAPIToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	_, claims, err := v.Verify(m.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Identity != "sam-1a2b" || claims.Name != "Sam" || claims.Video.Room != "standup" || !claims.Video.RoomJoin {
		t.Fatalf("claims %+v / %+v", claims, claims.Video)
	}
	if _, err := (Minter{}).Mint(Grant{Room: "r", Identity: "i"}); err == nil {
		t.Fatal("minted without a key")
	}
}
