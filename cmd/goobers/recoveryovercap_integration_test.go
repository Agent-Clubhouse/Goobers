//go:build integration

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// lowerCap reproduces the one state every reclaiming and observing reader used
// to refuse: an inventory already holding MORE entries than the operator cap.
// An operator reaches it by lowering retention.recovery.maxSnapshots on a full
// inventory, and the production wedge reached it the other way ("130 of 128").
//
// The cap is written to instance.yaml as well as to the carried configuration
// because resolveRecoveryPolicy and the occupancy reader both resolve it from
// that file at the point of use; the two must agree, or the test would be
// measuring a policy disagreement rather than over-cap behaviour.
func (f *reclaimFixture) lowerCap(limit int) {
	f.t.Helper()
	f.cap = limit
	f.cfg.Retention.Recovery = &instance.RecoverySnapshotConfig{MaxSnapshots: limit}
	// Only the retention section: the repository credentials the fixture
	// carries in memory are not expressible in a written instance.yaml, and
	// recovery policy is the only thing any path resolves from that file.
	written := &instance.Config{Retention: instance.RetentionConfig{
		Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: limit},
	}}
	if err := instance.WriteConfig(f.layout.ConfigFile(), written); err != nil {
		f.t.Fatal(err)
	}
}

// occupancyLine renders the `goobers status` inventory occupancy line for the
// fixture's instance.
func (f *reclaimFixture) occupancyLine() string {
	f.t.Helper()
	var out bytes.Buffer
	printRecoveryInventoryOccupancy(&out, f.layout)
	return strings.TrimSpace(out.String())
}

// TestIntegrationRecoveryReclaimsInventoryAlreadyOverItsCap is #5354's second
// acceptance bullet. An inventory holding more entries than the cap could not
// reclaim anything: recoveryEvictFunc and retireExpiredRecovery both read at
// the operator cap, and that read REFUSES on the count, so no reclamation rule
// was ever evaluated and every refused publish fell through to the overflow
// tier while superseded duplicates sat in the inventory untouched.
//
// The reclaimable entries here are an explicitly abandoned record (the first
// ordered rule, and one gated behind a terminal run exactly as the
// landing-proof rule is) and a set of superseded duplicates. Landing proof
// itself needs a restored-and-merged commit in the managed mirror, which
// internal/recovery/restore_integration_test.go covers at its own level; what
// this test pins is that the ordered rules run at all on an over-cap
// inventory, including one behind the terminal-run gate.
func TestIntegrationRecoveryReclaimsInventoryAlreadyOverItsCap(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 5)
	duplicate := map[string]string{"duplicate.txt": "identical agent work\n"}
	f.seed([]reclaimEntry{
		{runID: "overcap-abandoned", ageHours: 9, terminal: true, abandon: true,
			files: map[string]string{"abandoned.txt": "real but abandoned work\n"}},
		{runID: "overcap-dup-oldest", ageHours: 8, terminal: true, files: duplicate},
		{runID: "overcap-dup-middle", ageHours: 7, terminal: true, files: duplicate},
		{runID: "overcap-dup-newest", ageHours: 6, terminal: true, files: duplicate},
		{runID: "overcap-unique", ageHours: 5, terminal: true,
			files: map[string]string{"unique.txt": "distinct unlanded work\n"}},
	})
	f.lowerCap(3)

	// Observation first: the occupancy an operator can see must be the
	// occupancy the daemon's own health alarm already names.
	if line := f.occupancyLine(); !strings.Contains(line, "recovery inventory: 5/3") {
		t.Fatalf("status reported %q for an inventory of 5 against a cap of 3", line)
	}
	if state := f.inventoryState(); state != readservice.RecoveryInventoryExhausted {
		t.Fatalf("an over-cap inventory sampled as %q, want %q", state, readservice.RecoveryInventoryExhausted)
	}

	// A refused publish must evict, not overflow.
	if err := f.cleanupNewRun("overcap-new-run"); err != nil {
		t.Fatalf("cleanup was refused although the over-cap inventory held reclaimable entries: %v", err)
	}
	active := f.activeRunIDs()
	if !active["overcap-new-run"] {
		t.Fatal("the new run's snapshot was not published after reclamation on an over-cap inventory")
	}
	if overflow := f.overflowRunIDs(); overflow["overcap-new-run"] {
		t.Fatal("the new capture fell through to the overflow tier although a reclaimable entry was present")
	}
	if active["overcap-abandoned"] {
		t.Fatal("the explicitly abandoned entry was not reclaimed on an over-cap inventory")
	}
	// A surplus of more than one entry has to drain in the pass that pays for
	// it. Retiring exactly one per refused cleanup left the publish refused
	// anyway, so the instance recovered at one entry per refused cleanup.
	for _, superseded := range []string{"overcap-dup-oldest", "overcap-dup-middle"} {
		if active[superseded] {
			t.Fatalf("the surplus did not drain: superseded duplicate %s survived", superseded)
		}
		if justification, found := f.reclamationJustification(superseded); !found || justification != reclaimSupersededDuplicate {
			t.Fatalf("%s was retired without the superseded-duplicate justification: found=%v justification=%q", superseded, found, justification)
		}
	}
	if !active["overcap-dup-newest"] {
		t.Fatal("the newest member of a duplicate set was retired instead of the older copies it protects")
	}
	if !active["overcap-unique"] {
		t.Fatal("a unique, unlanded patch was discarded to drain an over-cap inventory")
	}
}

// TestIntegrationRecoveryExpirySweepRunsOnAnOverCapInventory covers the other
// read #5354 names: retireExpiredRecovery read at the operator cap, so the
// retention sweep — including the contentless purge that ignores the
// first-enable grace window — returned "inventory is full" before considering
// a single entry, on exactly the inventories that needed draining.
//
// No publish happens here, so the only thing that can retire anything is the
// sweep itself.
func TestIntegrationRecoveryExpirySweepRunsOnAnOverCapInventory(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 4)
	duplicate := map[string]string{"duplicate.txt": "identical agent work\n"}
	f.seed([]reclaimEntry{
		{runID: "sweep-overcap-nodiff", ageHours: 9, terminal: true},
		{runID: "sweep-overcap-dup-oldest", ageHours: 8, terminal: true, files: duplicate},
		{runID: "sweep-overcap-dup-newest", ageHours: 7, terminal: true, files: duplicate},
		{runID: "sweep-overcap-unique", ageHours: 6, terminal: true,
			files: map[string]string{"unique.txt": "distinct unlanded work\n"}},
	})
	f.lowerCap(2)

	if line := f.occupancyLine(); !strings.Contains(line, "recovery inventory: 4/2") {
		t.Fatalf("status reported %q for an inventory of 4 against a cap of 2", line)
	}
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	active := f.activeRunIDs()
	for justification, runID := range map[string]string{
		reclaimStoredNoDiff:        "sweep-overcap-nodiff",
		reclaimSupersededDuplicate: "sweep-overcap-dup-oldest",
	} {
		if active[runID] {
			t.Fatalf("the sweep retired nothing on an over-cap inventory: %s survived", runID)
		}
		if recorded, found := f.reclamationJustification(runID); !found || recorded != justification {
			t.Fatalf("%s was retired without the %s justification: found=%v justification=%q", runID, justification, found, recorded)
		}
	}
	if !active["sweep-overcap-dup-newest"] || !active["sweep-overcap-unique"] {
		t.Fatalf("the sweep discarded content it could not justify discarding: %v", active)
	}
}

// TestIntegrationRecoveryOverCapUniqueWorkStillOverflows is the safety half:
// reading past the cap must not turn "nothing is reclaimable" into a discard.
// When every entry on an over-cap inventory holds real, unique, unlanded work,
// the existing behaviour is preserved exactly — nothing is retired and the new
// capture is held at the ref tier.
func TestIntegrationRecoveryOverCapUniqueWorkStillOverflows(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 3)
	f.seed([]reclaimEntry{
		{runID: "overcap-only-a", ageHours: 5, terminal: true, files: map[string]string{"a.txt": "distinct work a\n"}},
		{runID: "overcap-only-b", ageHours: 4, terminal: true, files: map[string]string{"b.txt": "distinct work b\n"}},
		{runID: "overcap-only-c", ageHours: 3, terminal: true, files: map[string]string{"c.txt": "distinct work c\n"}},
	})
	f.lowerCap(1)

	if err := f.cleanupNewRun("overcap-overflow-run"); err != nil {
		t.Fatalf("cleanup was refused although the capture could overflow to the ref tier: %v", err)
	}
	active := f.activeRunIDs()
	for _, kept := range []string{"overcap-only-a", "overcap-only-b", "overcap-only-c"} {
		if !active[kept] {
			t.Fatalf("agent-authored work was discarded to drain an over-cap inventory: %s", kept)
		}
		if _, found := f.reclamationJustification(kept); found {
			t.Fatalf("a unique, unlanded entry was reclaimed on an over-cap inventory: %s", kept)
		}
	}
	if active["overcap-overflow-run"] {
		t.Fatal("the new capture took an inventory slot on an inventory that was already over its cap")
	}
	if overflow := f.overflowRunIDs(); !overflow["overcap-overflow-run"] {
		t.Fatalf("the new capture was neither retained nor held at the ref tier: %v", overflow)
	}
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if after := f.activeRunIDs(); len(after) != len(active) {
		t.Fatalf("the sweep changed an over-cap inventory of unique work: before=%v after=%v", active, after)
	}
}

// inventoryState samples the daemon's own inventory health classification for
// the fixture's instance.
func (f *reclaimFixture) inventoryState() string {
	f.t.Helper()
	return newRecoveryInventoryGate(f.layout, f.cfg, nil).Sample(f.t.Context()).State
}

// retainedRecord reads the effective (sidecar-aware) record the inventory
// currently holds for runID.
func (f *reclaimFixture) retainedRecord(runID string) recovery.Record {
	f.t.Helper()
	entries, _, err := recovery.ReadInventoryTolerant(f.t.Context(), f.root, recovery.MaxInventoryEntries)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Record.RunID != runID {
			continue
		}
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			f.t.Fatal(err)
		}
		return record
	}
	f.t.Fatalf("no inventory record for run %s", runID)
	return recovery.Record{}
}

// TestIntegrationTerminalFinalizationRunsOnAnOverCapInventory is the live
// follow-up on the same defect class. renewTerminalRecovery and
// terminalCaptureCovered both read the inventory bounded by the operator cap,
// and both only ever touch their own run's records. On an over-cap inventory
// the read refused ("10 of 8 slots used"), the failure was joined as
// worktree.ErrCleanupDeferred, and terminal finalization of EVERY completed run
// was deferred and retried at every daemon startup — each deferral holding the
// worktree and active marker it was trying to release.
func TestIntegrationTerminalFinalizationRunsOnAnOverCapInventory(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 4)
	f.seed([]reclaimEntry{
		{runID: "finalize-owner", ageHours: 9, terminal: true,
			files: map[string]string{"owner.txt": "work this run owns\n"}},
		{runID: "finalize-other-a", ageHours: 8, terminal: true,
			files: map[string]string{"a.txt": "work another run owns\n"}},
		{runID: "finalize-other-b", ageHours: 7, terminal: true,
			files: map[string]string{"b.txt": "more work another run owns\n"}},
		{runID: "finalize-other-c", ageHours: 6, terminal: true,
			files: map[string]string{"c.txt": "still more work another run owns\n"}},
	})
	f.lowerCap(2)

	before := f.retainedRecord("finalize-owner")
	if err := finalizeTerminalRun(f.layout, nil, f.manager, "finalize-owner"); err != nil {
		t.Fatalf("terminal finalization was deferred on an over-cap inventory: %v", err)
	}
	after := f.retainedRecord("finalize-owner")
	if !after.RetainUntil.After(before.RetainUntil) {
		t.Fatalf("terminal renewal did not extend the run's deadline: before=%s after=%s", before.RetainUntil, after.RetainUntil)
	}

	// A run with no record of its own has nothing to renew. That is a no-op,
	// never a refusal, whatever the rest of the inventory holds.
	f.seedRun(reclaimEntry{runID: "finalize-recordless", ageHours: 5, terminal: true})
	if err := finalizeTerminalRun(f.layout, nil, f.manager, "finalize-recordless"); err != nil {
		t.Fatalf("terminal finalization of a run holding no recovery record was deferred: %v", err)
	}
}
