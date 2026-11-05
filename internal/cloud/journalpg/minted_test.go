package journalpg

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestMintReservationCoversPublicationAndExpiresOldAuthority(t *testing.T) {
	key := journal.RunKey{Gaggle: "g", RunID: "run"}
	b := &Backend{
		runs:       map[journal.RunHandle]*runOwner{},
		minting:    map[journal.RunKey]bool{},
		unresolved: map[journal.RunKey]mintedOwner{key: {token: "old", epoch: 1}},
	}
	if !b.beginMint(key, 2) {
		t.Fatal("first mint refused")
	}
	if len(b.unresolved) != 0 {
		t.Fatal("previous epoch uncertainty survived")
	}
	if b.beginMint(key, 2) {
		t.Fatal("concurrent mint admitted before publication")
	}
	b.keepRun(&runOwner{key: key, token: "new", epoch: 2})
	if b.beginMint(key, 2) {
		t.Fatal("concurrent mint admitted during publication")
	}
	b.endMint(key)
	if b.beginMint(key, 2) {
		t.Fatal("published owner was ignored")
	}
	if !b.beginMint(key, 3) {
		t.Fatal("old incarnation survived epoch transfer")
	}
	b.endMint(key)
}
