//go:build integration

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestIntegrationRecoveryExpirySweepAppliesContentlessJustifications is the
// #5359 follow-up #5366 left open: recoveryRetirementEligible must apply
// recoveryContentlessJustification exactly as the on-demand eviction hook
// does, so the periodic sweep and the hook cannot disagree about what counts
// as contentless. Every case here seeds an entry well INSIDE its retain-until
// window (29 days out), so a retirement can only be explained by
// recoveryContentlessJustification, never by the sweep's own time-based
// policy.
func TestIntegrationRecoveryExpirySweepAppliesContentlessJustifications(t *testing.T) {
	t.Run("stored-no-diff", testSweepStoredNoDiff)
	t.Run("bookkeeping-only", testSweepBookkeepingOnly)
	t.Run("superseded-duplicate", testSweepSupersededDuplicate)
	t.Run("unique-patch-kept", testSweepUniquePatchKept)
	t.Run("dry-run-reports-only", testSweepDryRunReportsOnly)
}

func testSweepStoredNoDiff(t *testing.T) {
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{{runID: "nodiff-sweep", ageHours: 3, terminal: true}})
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if f.activeRunIDs()["nodiff-sweep"] {
		t.Fatal("a stored-no-diff entry inside the retain window was not retired by the sweep")
	}
	justification, found := f.reclamationJustification("nodiff-sweep")
	if !found || justification != reclaimStoredNoDiff {
		t.Fatalf("sweep retirement was not journaled with the stored-no-diff justification: found=%v justification=%q", found, justification)
	}
}

func testSweepBookkeepingOnly(t *testing.T) {
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "bookkeeping-sweep", ageHours: 3, terminal: true, files: map[string]string{"claimed-item.json": "{}\n"}},
	})
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if f.activeRunIDs()["bookkeeping-sweep"] {
		t.Fatal("a bookkeeping-only entry inside the retain window was not retired by the sweep")
	}
	justification, found := f.reclamationJustification("bookkeeping-sweep")
	if !found || justification != reclaimBookkeepingOnly {
		t.Fatalf("sweep retirement was not journaled with the bookkeeping-only justification: found=%v justification=%q", found, justification)
	}
}

func testSweepSupersededDuplicate(t *testing.T) {
	f := newReclaimFixture(t, 3)
	content := map[string]string{"duplicate.txt": "identical agent work\n"}
	f.seed([]reclaimEntry{
		{runID: "dup-oldest-sweep", ageHours: 5, terminal: true, files: content},
		{runID: "dup-newest-sweep", ageHours: 3, terminal: true, files: content},
	})
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	active := f.activeRunIDs()
	if active["dup-oldest-sweep"] {
		t.Fatal("the older duplicate was not retired by the sweep")
	}
	if !active["dup-newest-sweep"] {
		t.Fatal("the newest member of a duplicate set was retired instead of the older copy it protects")
	}
	justification, found := f.reclamationJustification("dup-oldest-sweep")
	if !found || justification != reclaimSupersededDuplicate {
		t.Fatalf("sweep retirement was not journaled with the superseded-duplicate justification: found=%v justification=%q", found, justification)
	}
}

func testSweepUniquePatchKept(t *testing.T) {
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "unique-sweep", ageHours: 3, terminal: true, files: map[string]string{"unique.txt": "distinct unlanded work\n"}},
	})
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !f.activeRunIDs()["unique-sweep"] {
		t.Fatal("a unique, unlanded patch inside the retain window was retired by the sweep")
	}
	if _, found := f.reclamationJustification("unique-sweep"); found {
		t.Fatal("a kept entry must not be journaled as reclaimed")
	}
}

func testSweepDryRunReportsOnly(t *testing.T) {
	f := newReclaimFixture(t, 3)
	f.seed([]reclaimEntry{
		{runID: "nodiff-dry", ageHours: 3, terminal: true},
		{runID: "bookkeeping-dry", ageHours: 2, terminal: true, files: map[string]string{"claimed-item.json": "{}\n"}},
	})
	stdout, err := f.runExpirySweep(true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	active := f.activeRunIDs()
	if !active["nodiff-dry"] || !active["bookkeeping-dry"] {
		t.Fatal("dry-run deleted a candidate instead of only reporting it")
	}
	for _, want := range []string{reclaimStoredNoDiff, reclaimBookkeepingOnly} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run output did not name candidate rule %q: %s", want, stdout)
		}
	}
	if _, found := f.reclamationJustification("nodiff-dry"); found {
		t.Fatal("dry-run must not journal a reclamation")
	}
}

// runExpirySweep drives the real periodic retention sweep
// (pruneConfiguredRetention) against the fixture's inventory, with retention
// enabled and the first-enable grace window skipped, so every assertion is
// about what pruneConfiguredRetention actually did on disk. dryRun toggles
// the sweep's own dry-run override.
func (f *reclaimFixture) runExpirySweep(dryRun bool) (string, error) {
	f.t.Helper()
	cfg := *f.cfg
	cfg.Retention.Enabled = boolPtr(true)
	cfg.Retention.FirstEnable = "immediate"
	cfg.Retention.DryRun = dryRun
	setup := &schedulerSetup{Config: &cfg, LegacyWorktrees: f.manager}
	var stdout, stderr bytes.Buffer
	err := pruneConfiguredRetention(f.t.Context(), f.layout, setup, &stdout, &stderr)
	if err != nil {
		f.t.Logf("sweep stderr: %s", stderr.String())
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// reclamationJustification reports the recoveryReclamationJustification a
// recovery-reclaimed annotation recorded for runID, if the sweep journaled
// one. The sweep must journal exactly like recoveryEvictionScope.
// journalReclamation does for the on-demand hook (recoveryevict.go), so a
// contentless retirement is equally explainable regardless of which path
// retired it.
func (f *reclaimFixture) reclamationJustification(runID string) (string, bool) {
	f.t.Helper()
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID != runID || event.Runner == nil {
			continue
		}
		if event.Runner["operation"] != "recovery-reclaimed" {
			continue
		}
		justification, _ := event.Runner["recoveryReclamationJustification"].(string)
		return justification, true
	}
	return "", false
}
