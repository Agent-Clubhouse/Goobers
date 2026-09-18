//go:build integration

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationRecoveryOverflowKeepsCleanupSucceeding is #5370's central
// acceptance: an inventory full of unique, unlanded, in-window captures — the
// case where every reclamation rule correctly declines — must still let the
// next real worktree cleanup succeed. Before the overflow tier the cleanup was
// refused, its run branch could not be reacquired, and unrelated runs then
// failed at `create worktree`.
func TestIntegrationRecoveryOverflowKeepsCleanupSucceeding(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	seeded := f.seed([]reclaimEntry{
		{runID: "unique-a", ageHours: 3, files: map[string]string{"a.txt": "distinct work a\n"}},
		{runID: "unique-b", ageHours: 2, files: map[string]string{"b.txt": "distinct work b\n"}},
	})
	before := f.activeRunIDs()
	if err := f.cleanupNewRun("overflow-run"); err != nil {
		t.Fatalf("cleanup was refused although the capture could overflow to the ref tier: %v", err)
	}
	if after := f.activeRunIDs(); len(after) != len(before) || !after["unique-a"] || !after["unique-b"] {
		t.Fatalf("overflow disturbed the retained entries: before=%v after=%v", before, after)
	}
	for _, entry := range seeded {
		current, err := recovery.ReadRetainedRecord(filepath.Join(f.root, recoveryTestEntryDirectory(t, f.root, entry.published), recovery.RecordFileName))
		if err != nil || current.SnapshotSHA != entry.published.SnapshotSHA {
			t.Fatalf("a retained entry changed during overflow: %+v %v", current, err)
		}
	}
	overflow := f.overflowRecords()
	if len(overflow) != 1 || overflow[0].RunID != "overflow-run" {
		t.Fatalf("the refused capture was not published to the overflow tier: %+v", overflow)
	}
	if !f.journaledOverflow("overflow-run") {
		t.Fatal("the overflow publication was not journaled with recoveryOverflow=true")
	}
	// The ref is the whole durability claim of the tier: without it the record
	// names work that cannot be restored.
	if !recovery.HasSnapshotRef(t.Context(), f.mirror, overflow[0]) {
		t.Fatal("the overflow record names a snapshot ref the mirror does not hold")
	}
}

// TestIntegrationRecoveryOnFullRefuseKeepsTheOldRefusal pins the escape hatch:
// an operator who would rather wedge execution than hold work at the ref tier
// still gets the pre-#5370 fail-closed behaviour, unchanged.
func TestIntegrationRecoveryOnFullRefuseKeepsTheOldRefusal(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.cfg.Retention.Recovery.OnFull = instance.RecoveryOnFullRefuse
	f.seed([]reclaimEntry{
		{runID: "refuse-a", ageHours: 3, files: map[string]string{"a.txt": "distinct work a\n"}},
		{runID: "refuse-b", ageHours: 2, files: map[string]string{"b.txt": "distinct work b\n"}},
	})
	err := f.cleanupNewRun("refused-run")
	if err == nil {
		t.Fatal("onFull: refuse acknowledged a cleanup the inventory had no room for")
	}
	if !errors.Is(err, recovery.ErrInventoryFull) {
		t.Fatalf("onFull: refuse failed for some reason other than capacity: %v", err)
	}
	if records := f.overflowRecords(); len(records) != 0 {
		t.Fatalf("onFull: refuse still wrote to the overflow tier: %+v", records)
	}
}

// TestIntegrationRecoveryOverflowPromotesOldestFirst covers the convergence
// half of the design: overflow is a holding tier, not a destination. As slots
// free, the retention pass turns entries back into bundles oldest capture
// first, and only as many as there is room for.
func TestIntegrationRecoveryOverflowPromotesOldestFirst(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 3)
	f.seed([]reclaimEntry{
		{runID: "hold-a", ageHours: 9, terminal: true, files: map[string]string{"a.txt": "held a\n"}},
		{runID: "hold-b", ageHours: 8, terminal: true, files: map[string]string{"b.txt": "held b\n"}},
		{runID: "hold-c", ageHours: 7, terminal: true, files: map[string]string{"c.txt": "held c\n"}},
	})
	// Three overflow entries, deliberately published newest-last so that
	// directory order and age order disagree.
	f.overflowSeed("over-oldest", 6, "oldest overflow\n")
	f.overflowSeed("over-middle", 5, "middle overflow\n")
	f.overflowSeed("over-newest", 4, "newest overflow\n")

	// One free slot: exactly one promotion, and it must be the oldest.
	f.retireSeeded("hold-a")
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if active := f.activeRunIDs(); !active["over-oldest"] {
		t.Fatalf("the oldest overflow entry was not promoted into the freed slot: %v", active)
	}
	if remaining := f.overflowRunIDs(); len(remaining) != 2 || remaining["over-oldest"] {
		t.Fatalf("promotion did not remove exactly the promoted overflow record: %v", remaining)
	}

	// Two more free slots: both remaining entries promote, in age order.
	f.retireSeeded("hold-b")
	f.retireSeeded("hold-c")
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if remaining := f.overflowRunIDs(); len(remaining) != 0 {
		t.Fatalf("two free slots did not promote both remaining overflow entries: %v", remaining)
	}
	active := f.activeRunIDs()
	for _, runID := range []string{"over-oldest", "over-middle", "over-newest"} {
		if !active[runID] {
			t.Fatalf("%s is neither retained nor in overflow after promotion: %v", runID, active)
		}
	}
}

// TestIntegrationRecoveryOverflowRetiresOnExplicitAbandonment pins #5354
// bullet 7 for the new tier from the other side: an overflow entry is not
// beyond an operator's reach. An explicit abandonment retires it exactly as it
// retires a bundled snapshot — ref unpinned, record gone.
func TestIntegrationRecoveryOverflowRetiresOnExplicitAbandonment(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.seed([]reclaimEntry{
		{runID: "keep-a", ageHours: 3, terminal: true, files: map[string]string{"a.txt": "kept a\n"}},
		{runID: "keep-b", ageHours: 2, terminal: true, files: map[string]string{"b.txt": "kept b\n"}},
	})
	record := f.overflowSeed("abandon-me", 4, "work the operator gave up on\n")
	f.abandon(record)
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if remaining := f.overflowRunIDs(); remaining["abandon-me"] {
		t.Fatalf("an explicitly abandoned overflow entry survived the sweep: %v", remaining)
	}
	if recovery.HasSnapshotRef(t.Context(), f.mirror, record) {
		t.Fatal("retiring an overflow entry left its snapshot ref pinned in the mirror")
	}
}

// overflowSeed publishes one overflow entry for a terminal run, the way a
// cleanup that found the inventory full would have.
func (f *reclaimFixture) overflowSeed(runID string, ageHours int, content string) recovery.Record {
	f.t.Helper()
	entry := reclaimEntry{runID: runID, ageHours: ageHours, terminal: true, files: map[string]string{runID + ".txt": content}}
	snapshot := f.commitSnapshot(entry)
	// The mirror must learn the commit, exactly as seed() notes: only the
	// mirror and pinned clones are ever visited.
	recoveryCLIGit(f.t, f.mirror, "fetch", "--no-tags", "--", f.source, "+refs/heads/*:refs/heads/*")
	record := f.overflowRecord(entry, snapshot)
	published, _, err := recovery.PublishOverflow(f.t.Context(), f.mirror, recoveryOverflowRoot(f.layout), record)
	if err != nil {
		f.t.Fatal(err)
	}
	f.seedRun(entry)
	return published
}

func (f *reclaimFixture) overflowRecord(entry reclaimEntry, snapshot string) recovery.Record {
	f.t.Helper()
	ref, err := recovery.RefForSnapshot(entry.runID, snapshot)
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := recovery.WriteSnapshotPatch(f.t.Context(), f.mirror, f.base, snapshot, io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	createdAt := time.Now().Add(-time.Duration(entry.ageHours) * time.Hour)
	return recovery.Record{
		Version: 1, RunID: entry.runID, RepositoryKey: f.key, Ref: ref,
		BaseSHA: f.base, SnapshotSHA: snapshot, PatchDigest: digest,
		CreatedAt: createdAt, RetainUntil: createdAt.Add(29 * 24 * time.Hour),
	}
}

func (f *reclaimFixture) overflowRecords() []recovery.Record {
	f.t.Helper()
	entries, unreadable, err := readConfiguredRecoveryOverflow(f.t.Context(), f.layout)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(unreadable) != 0 {
		f.t.Fatalf("overflow tier holds unreadable entries: %+v", unreadable)
	}
	records := make([]recovery.Record, 0, len(entries))
	for _, entry := range entries {
		records = append(records, entry.Record)
	}
	return records
}

func (f *reclaimFixture) overflowRunIDs() map[string]bool {
	f.t.Helper()
	present := map[string]bool{}
	for _, record := range f.overflowRecords() {
		present[record.RunID] = true
	}
	return present
}

// retireSeeded frees exactly one inventory slot by retiring and reaping a
// seeded entry, so a promotion has somewhere to go.
func (f *reclaimFixture) retireSeeded(runID string) {
	f.t.Helper()
	entries, err := recovery.ReadInventory(f.t.Context(), f.root, recovery.MaxInventoryEntries)
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
		if _, err := recovery.RetireSnapshot(f.t.Context(), f.root, record, func(current recovery.Record) error {
			return recovery.DeleteSnapshotRef(f.t.Context(), f.mirror, current)
		}); err != nil {
			f.t.Fatal(err)
		}
		if _, err := recovery.ReapRetired(f.t.Context(), f.root, recovery.MaxInventoryEntries, true); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	f.t.Fatalf("no retained entry for %s", runID)
}

// journaledOverflow reports whether the capture acknowledgement for runID
// declared the overflow tier. Without that marker nothing downstream can tell
// ref-durable work from bundle-durable work.
func (f *reclaimFixture) journaledOverflow(runID string) bool {
	f.t.Helper()
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Runner["recoveryOverflow"] == true {
			return true
		}
	}
	return false
}

// recoveryTestEntryDirectory locates a published record's reservation by
// reading the inventory back, rather than by recomputing the identity hash the
// implementation uses — a test that recomputed it could not catch the
// implementation moving an entry.
func recoveryTestEntryDirectory(t *testing.T, root string, record recovery.Record) string {
	t.Helper()
	names, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		current, err := recovery.ReadRecord(filepath.Join(root, name.Name(), recovery.RecordFileName))
		if err == nil && current.SnapshotSHA == record.SnapshotSHA && current.RunID == record.RunID {
			return name.Name()
		}
	}
	t.Fatalf("no reservation holds %s", record.Ref)
	return ""
}
