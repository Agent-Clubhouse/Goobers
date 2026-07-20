package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestClaimLedgerJournalsTransitions(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(filepath.Join(dir, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	l, err := OpenClaimLedger(filepath.Join(dir, "claims.json"), WithInstanceLog(log))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("issue-8", "run-a", "curate", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Release("issue-8", "run-a"); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(filepath.Join(dir, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 journaled transitions, got %d: %+v", len(events), events)
	}
	if events[0].Type != journal.EventClaimAcquired || events[0].Name != "issue-8" || events[0].RunID != "run-a" {
		t.Errorf("claim.acquired not journaled correctly: %+v", events[0])
	}
	if events[1].Type != journal.EventClaimReleased || events[1].Name != "issue-8" {
		t.Errorf("claim.released not journaled correctly: %+v", events[1])
	}
}
