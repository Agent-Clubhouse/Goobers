package scheduling

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func testClaimLifecycle() ClaimLifecycle {
	return ClaimLifecycle{
		Files:          ClaimFiles{BoundedLock: testNamedLock},
		SharedResolver: func(instance.Layout) claimsclient.SharedClaimResolver { return nil },
		IsReconcile:    func(string) bool { return false },
	}
}
func TestReleaseClaimsForRunReleasesAllOwnedClaims(t *testing.T) {
	l := newClaimTestLayout(t)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	ledgerPath := filepath.Join(l.SchedulerDir(), "claims.json")
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"7", "8", "9"} {
		if ok, _, err := ledger.Claim(itemID, "terminal-run", "backlog-curation", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", itemID, ok, err)
		}
	}
	if ok, _, err := ledger.Claim("10", "other-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed other claim: ok=%v err=%v", ok, err)
	}

	if err := testClaimLifecycle().ReleaseForRun(l, log, "terminal-run", func(op string, fn func() error) error {
		return testNamedLock(filepath.Join(l.SchedulerDir(), "claims.lock"), op, fn)
	}); err != nil {
		t.Fatal(err)
	}
	if err := testClaimLifecycle().ReleaseForRun(l, log, "terminal-run", func(op string, fn func() error) error {
		return testNamedLock(filepath.Join(l.SchedulerDir(), "claims.lock"), op, fn)
	}); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entries := reopened.ForRunAll("terminal-run"); len(entries) != 0 {
		t.Fatalf("terminal run still holds claims: %+v", entries)
	}
	if entry, ok := reopened.Lookup("10"); !ok || entry.RunID != "other-run" {
		t.Fatalf("other run's claim = (%+v, %v), want preserved", entry, ok)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var released int
	for _, event := range events {
		if event.Type == journal.EventClaimReleased && event.RunID == "terminal-run" {
			released++
		}
	}
	if released != 3 {
		t.Fatalf("claim.released events = %d, want 3", released)
	}
	// No run journal exists for this holder: the cause is recorded as
	// deliberately unknown, never guessed (#4421).
	assertClaimReleaseReason(t, reopened, "terminal-run", claimrelease.Unknown)
}

// assertClaimReleaseReason pins the disposition terminal cleanup recorded on
// every released history entry of runID.
func assertClaimReleaseReason(t *testing.T, ledger *localscheduler.ClaimLedger, runID string, want claimrelease.Disposition) {
	t.Helper()
	history := ledger.HistoryForRun(runID)
	if len(history) == 0 {
		t.Fatalf("no claim history for %s", runID)
	}
	for _, entry := range history {
		if entry.ReleasedAt == nil || entry.ReleaseReason.Disposition != want {
			t.Fatalf("release reason for %s/%s = %+v, want %s", runID, entry.ItemID, entry.ReleaseReason, want)
		}
	}
}
