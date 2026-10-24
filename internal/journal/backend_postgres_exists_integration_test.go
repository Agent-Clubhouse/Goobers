//go:build cloudintegration

package journal_test

import (
	"errors"
	"os"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testdep"
)

func TestPostgresCreateExistingRunReturnsErrExist(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	config := postgresJournalConfig(t)
	var role string
	if err := config.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("runtime role=%q: %v", role, err)
	}
	b := postgresBackend(t, config)
	request := journal.CreateRunRequest{Identity: journal.RunIdentity{RunID: "run-create-exists", Gaggle: "g", Workflow: "test"}}
	h, owner, err := b.CreateRun(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// A separate backend has no in-process reservation, so it reaches the
	// journal.run INSERT and must normalize PostgreSQL's unique violation.
	duplicate := postgresBackend(t, config)
	notified := false
	request.Options.AppendObserver = func(string, uint64) { notified = true }
	for _, retired := range []bool{false, true} {
		if retired {
			if err := b.Retire(t.Context(), h, owner); err != nil {
				t.Fatal(err)
			}
		}
		gotHandle, gotOwner, err := duplicate.CreateRun(t.Context(), request)
		if !errors.Is(err, os.ErrExist) || gotHandle != "" || gotOwner != "" || notified {
			t.Fatalf("existing run (retired=%v): handle=%q owner set=%v err=%v notified=%v", retired, gotHandle, gotOwner != "", err, notified)
		}
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: request.Identity.RunID}, 0, 0)
	if err != nil || len(records) != 1 || records[0].Event.Type != journal.EventRunStarted {
		t.Fatalf("duplicate create changed original history: %v %v", records, err)
	}
}
