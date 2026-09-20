//go:build integration

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// activeRunIDsTolerant is f.activeRunIDs but via the tolerant read: these
// tests deliberately leave an incomplete reservation (no record.json) on disk
// after the sweep, and the strict read f.activeRunIDs uses fails closed on
// exactly that shape (that failure is the wedge #5354 exists to fix
// elsewhere, not something this helper should reproduce).
func (f *reclaimFixture) activeRunIDsTolerant() map[string]bool {
	f.t.Helper()
	entries, _, err := recovery.ReadInventoryTolerant(f.t.Context(), f.root, recovery.MaxInventoryEntries)
	if err != nil {
		f.t.Fatal(err)
	}
	active := make(map[string]bool, len(entries))
	for _, entry := range entries {
		active[entry.Record.RunID] = true
	}
	return active
}

// TestIntegrationRecoveryGraceWindowDeletesContentlessDespiteGrace is #5354
// acceptance bullet 2: an instance already wedged with contentless entries and
// incomplete reservations must recover on its FIRST pass after upgrade, not
// seven days later. The first-enable grace window (#4253) exists to give an
// operator time to review real worktrees and snapshots before deletion; a
// contentless retirement (stored-no-diff, bookkeeping-only,
// superseded-duplicate) and an incomplete reservation with no record.json
// hold nothing reviewable, so they must be deleted on this very pass even
// though the grace window has not elapsed. An entry holding a unique,
// unlanded patch inside the retain window is the control: it must still only
// be REPORTED, because the grace window's protection is exactly for entries
// like it.
func TestIntegrationRecoveryGraceWindowDeletesContentlessDespiteGrace(t *testing.T) {
	testdep.Require(t, "git")
	// Capacity covers 3 published entries plus the 2 incomplete-reservation
	// directories seeded below: the inventory read compares directory COUNT
	// against the cap, and reading at a smaller cap than the debris on disk
	// is #5354's separate "130 of 128" failure mode, not what this test is
	// pinning.
	f := newReclaimFixture(t, 5)
	f.seed([]reclaimEntry{
		{runID: "nodiff-grace", ageHours: 3, terminal: true},
		{runID: "bookkeeping-grace", ageHours: 3, terminal: true, files: map[string]string{"claimed-item.json": "{}\n"}},
		{runID: "unique-grace", ageHours: 3, terminal: true, files: map[string]string{"unique.txt": "distinct unlanded work\n"}},
	})
	stale := seedIncompleteRecoveryReservation(t, f.root, 900, 4*recovery.IncompleteReservationGrace)
	fresh := seedIncompleteRecoveryReservation(t, f.root, 901, 0)

	// The sweep's own recovery-entry scan reports an incomplete reservation
	// (no record.json) as an unreadable inventory entry and folds that into
	// its returned error — the same non-fatal reporting
	// TestConfiguredRetentionReconcilesIncompleteRecoveryReservations already
	// exercises directly. reconcileIncompleteRecovery is a separate pass over
	// the same directories that reclaims them; this test's assertions are
	// about outcomes on disk, not about pruneConfiguredRetention returning
	// nil.
	stdout, err := f.runGraceSweep(false)
	if err != nil && !strings.Contains(err.Error(), "unreadable recovery reservation") {
		t.Fatalf("sweep failed for an unexpected reason: %v", err)
	}

	active := f.activeRunIDsTolerant()
	if active["nodiff-grace"] {
		t.Fatal("a stored-no-diff entry was held by the grace window instead of being reclaimed immediately")
	}
	if active["bookkeeping-grace"] {
		t.Fatal("a bookkeeping-only entry was held by the grace window instead of being reclaimed immediately")
	}
	if !active["unique-grace"] {
		t.Fatal("an entry holding a unique, unlanded patch was deleted despite the first-enable grace window")
	}
	if _, found := f.reclamationJustification("unique-grace"); found {
		t.Fatal("an entry with real, unlanded content must never be journaled as a contentless reclamation")
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale incomplete reservation survived the grace window: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh, possibly in-flight reservation was reclaimed: %v", err)
	}

	nodiffJustification, found := f.reclamationJustification("nodiff-grace")
	if !found || nodiffJustification != reclaimStoredNoDiff {
		t.Fatalf("stored-no-diff retirement was not journaled: found=%v justification=%q", found, nodiffJustification)
	}
	bookkeepingJustification, found := f.reclamationJustification("bookkeeping-grace")
	if !found || bookkeepingJustification != reclaimBookkeepingOnly {
		t.Fatalf("bookkeeping-only retirement was not journaled: found=%v justification=%q", found, bookkeepingJustification)
	}

	// The pass must call out, in its own output, that it deleted despite an
	// active grace window: an operator watching this instance for the first
	// time after upgrade needs to see why anything at all was removed before
	// the window's usual seven days.
	if !strings.Contains(stdout, "grace window does not apply: no recoverable content") {
		t.Fatalf("no grace-window-override line was printed for the contentless deletions: %s", stdout)
	}
	graceOverrideLines := strings.Count(stdout, "grace window does not apply: no recoverable content")
	if graceOverrideLines < 2 {
		t.Fatalf("expected a grace-override line for both the recovery and incomplete-reservation deletions, got %d: %s", graceOverrideLines, stdout)
	}
	if !strings.Contains(stdout, "kind=recovery") {
		t.Fatalf("grace-override output did not name the recovery kind: %s", stdout)
	}
	if !strings.Contains(stdout, "kind=incomplete-recovery-reservation") {
		t.Fatalf("grace-override output did not name the incomplete-reservation kind: %s", stdout)
	}
}

// TestIntegrationRecoveryGraceWindowOperatorDryRunHoldsEverything is the
// counterpart safety case: the operator's own retention.dryRun always wins,
// even over "no recoverable content". Nothing is deleted; everything is only
// reported.
func TestIntegrationRecoveryGraceWindowOperatorDryRunHoldsEverything(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 3)
	f.seed([]reclaimEntry{
		{runID: "nodiff-preview", ageHours: 3, terminal: true},
		{runID: "bookkeeping-preview", ageHours: 3, terminal: true, files: map[string]string{"claimed-item.json": "{}\n"}},
	})
	stale := seedIncompleteRecoveryReservation(t, f.root, 950, 4*recovery.IncompleteReservationGrace)

	// See the comment in TestIntegrationRecoveryGraceWindowDeletesContentlessDespiteGrace:
	// the incomplete reservation is expected to surface as a reported,
	// non-fatal "unreadable" entry from the sweep's own recovery-entry scan.
	stdout, err := f.runGraceSweep(true)
	if err != nil && !strings.Contains(err.Error(), "unreadable recovery reservation") {
		t.Fatalf("sweep failed for an unexpected reason: %v", err)
	}

	active := f.activeRunIDsTolerant()
	if !active["nodiff-preview"] || !active["bookkeeping-preview"] {
		t.Fatal("operator dry-run deleted a contentless entry instead of only reporting it")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("operator dry-run reclaimed a stale incomplete reservation: %v", err)
	}
	for _, want := range []string{reclaimStoredNoDiff, reclaimBookkeepingOnly} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("operator dry-run output did not name candidate rule %q: %s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "retention candidate kind=incomplete-recovery-reservation") {
		t.Fatalf("operator dry-run did not report the incomplete reservation as a candidate: %s", stdout)
	}
	if _, found := f.reclamationJustification("nodiff-preview"); found {
		t.Fatal("operator dry-run must not journal a reclamation")
	}
	if strings.Contains(stdout, "retention deleted") {
		t.Fatalf("operator dry-run reported a deletion: %s", stdout)
	}
}

// runGraceSweep drives the real periodic retention sweep with retention
// enabled and the first-enable grace window left at its default (unelapsed,
// #4253) — unlike runExpirySweep, which sets FirstEnable: "immediate" to skip
// it. operatorDryRun sets the operator's own retention.dryRun override, which
// always wins regardless of the window.
func (f *reclaimFixture) runGraceSweep(operatorDryRun bool) (string, error) {
	f.t.Helper()
	cfg := *f.cfg
	cfg.Retention.Enabled = boolPtr(true)
	cfg.Retention.DryRun = operatorDryRun
	setup := &schedulerSetup{Config: &cfg, LegacyWorktrees: f.manager}
	var stdout, stderr bytes.Buffer
	err := pruneConfiguredRetention(f.t.Context(), f.layout, setup, &stdout, &stderr)
	if err != nil {
		f.t.Logf("sweep stderr: %s", stderr.String())
		return stdout.String(), err
	}
	return stdout.String(), nil
}
