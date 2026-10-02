package podauth

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestLocalStartAuthorityIsNarrowAndDiesOnRestart(t *testing.T) {
	authority, err := NewLocalStartAuthority()
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{Type: journal.EventStageStarted, Seq: 7, Stage: "build", Attempt: 1}
	proof := authority.SealControllerStart("run", "op", event)
	if !authority.VerifyControllerStart("run", "op", event, proof) {
		t.Fatal("own start rejected")
	}
	restarted, err := NewLocalStartAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if restarted.VerifyControllerStart("run", "op", event, proof) {
		t.Fatal("ephemeral proof survived restart")
	}
	event.Seq++
	if authority.VerifyControllerStart("run", "op", event, proof) {
		t.Fatal("changed anchor accepted")
	}
	if _, ok := authority.(livejournal.ControllerJournalVerifier); ok {
		t.Fatal("local authority can authenticate controller HTTP")
	}
	if _, ok := authority.(Verifier); ok {
		t.Fatal("local authority can authenticate pod HTTP")
	}
	if _, ok := authority.(*SignedKey); ok {
		t.Fatal("reusable signer escaped")
	}
}
