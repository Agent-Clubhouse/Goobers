package readmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func completed(runID string, seq uint64) Projection {
	startedAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	finished := startedAt.Add(time.Minute)
	return Projection{Run: RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: seq,
	}}
}

// TestRebuildMintsANewEpochAndSwapsInPlace pins the happy path end to end.
func TestRebuildMintsANewEpochAndSwapsInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for i := 0; i < 5; i++ {
		if err := store.UpsertRun(ctx, completed(fmt.Sprintf("%032x", i), uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	// The live store stays readable while the rebuild runs — that is the point
	// of building beside it rather than in place.
	if page, err := store.ListRuns(ctx, ListOptions{Limit: 10}); err != nil || len(page.Runs) != 5 {
		t.Errorf("live store not readable during rebuild: %d runs, err=%v", len(page.Runs), err)
	}

	for i := 0; i < 5; i++ {
		if err := rebuild.Target().UpsertRun(ctx, completed(fmt.Sprintf("%032x", i), uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if err := rebuild.Swap(ctx); err != nil {
		t.Fatalf("swap: %v", err)
	}

	after, err := store.State(ctx)
	if err != nil {
		t.Fatalf("state after swap: %v", err)
	}
	if after.Epoch == before.Epoch {
		t.Error("the epoch did not change across a rebuild; a client holding a cursor from " +
			"the old store would be told to continue against the new one and would wait " +
			"forever for a sequence that will never arrive")
	}
	page, err := store.ListRuns(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list after swap: %v", err)
	}
	if len(page.Runs) != 5 {
		t.Errorf("after the swap the store holds %d runs, want 5", len(page.Runs))
	}

	// No leftovers: neither the build file nor the retained previous epoch.
	if _, err := os.Stat(rebuild.Path); !os.IsNotExist(err) {
		t.Errorf("the rebuild file survived the swap: %v", err)
	}
	if _, err := os.Stat(path + ".previous"); !os.IsNotExist(err) {
		t.Error("the previous epoch was retained after the swap was confirmed")
	}
}

// TestSwapRefusesToMoveARunBackwards is the regression guard, and the reason
// validation happens before anything is moved.
//
// If the rebuild holds a run at an earlier source position than the live store,
// publishing it rewinds that run for every client. A rebuild that loses ground
// is worse than no rebuild, so this aborts rather than publishes.
func TestSwapRefusesToMoveARunBackwards(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runID := fmt.Sprintf("%032x", 1)
	if err := store.UpsertRun(ctx, completed(runID, 50)); err != nil {
		t.Fatal(err)
	}
	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The rebuild only reached source position 20.
	if err := rebuild.Target().UpsertRun(ctx, completed(runID, 20)); err != nil {
		t.Fatal(err)
	}

	err = rebuild.Swap(ctx)
	if err == nil {
		t.Fatal("the swap published a rebuild that moves a run backwards")
	}
	t.Logf("refused: %v", err)

	// The live store is untouched and still serving.
	row, ok, err := store.GetRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("live store broken after a refused swap: ok=%v err=%v", ok, err)
	}
	if row.LastSeq != 50 {
		t.Errorf("live run is at source position %d after a refused swap, want 50", row.LastSeq)
	}
	_ = rebuild.Abort()
}

// TestSwapRefusesToDropANonTombstonedRun pins the other half of validation.
//
// A run absent from the rebuild is legitimate only if it was deliberately aged
// out. Otherwise the rebuild would silently drop it, and silence is exactly what
// §14.7 exists to prevent.
func TestSwapRefusesToDropANonTombstonedRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.UpsertRun(ctx, completed(fmt.Sprintf("%032x", 1), 5)); err != nil {
		t.Fatal(err)
	}
	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild projects nothing at all.
	if err := rebuild.Swap(ctx); err == nil {
		t.Error("the swap published a rebuild that silently dropped a run")
	}
	_ = rebuild.Abort()
}

// TestTombstonedRunMayBeAbsentFromTheRebuild is the exception to the above, and
// the reason the floor and tombstones are copied BEFORE any projection.
//
// They are derived policy state, not journal facts, so a rebuild from journals
// alone does not reproduce them. Without the copy, a post-retention rebuild
// would re-admit every expired journal, burst removals into the change feed, and
// make rebuild size proportional to total history rather than the retained
// window.
func TestTombstonedRunMayBeAbsentFromTheRebuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runID := fmt.Sprintf("%032x", 3)
	if err := store.UpsertRun(ctx, completed(runID, 5)); err != nil {
		t.Fatal(err)
	}
	aged := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Tombstone(ctx, runID, aged, "below_projection_floor"); err != nil {
		t.Fatal(err)
	}
	floor := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SetProjectionFloor(ctx, floor); err != nil {
		t.Fatal(err)
	}

	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The policy state must already be in the target, before any projection.
	if tombstoned, err := rebuild.Target().Tombstoned(ctx, runID); err != nil {
		t.Fatal(err)
	} else if !tombstoned {
		t.Error("tombstones were not copied into the new epoch before projecting; a " +
			"post-retention rebuild would re-admit every expired journal")
	}
	if copied, ok, err := rebuild.Target().ProjectionFloor(ctx); err != nil {
		t.Fatal(err)
	} else if !ok || !copied.Equal(floor) {
		t.Errorf("the projection floor was not copied into the new epoch: %v ok=%v", copied, ok)
	}

	if err := rebuild.Swap(ctx); err != nil {
		t.Fatalf("a rebuild omitting a tombstoned run must be allowed to swap: %v", err)
	}
}

// TestCatchUpUsesTheChangeFeedNotOnlyPendingIntake is the fixture #1925 names.
//
// The shortcut "replay whatever intake still has pending" loses data invisibly:
//
//	E reads run R at source seq 10.
//	R advances to 11 while E builds.
//	The old epoch — still live — projects 11 and ACKNOWLEDGES R's marker.
//	The barrier sees no pending marker, and publishes E stale at 10.
//
// Nothing reports it. The change feed is the second source precisely because it
// records what the old epoch APPLIED, whether or not the marker survived.
func TestCatchUpUsesTheChangeFeedNotOnlyPendingIntake(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runID := fmt.Sprintf("%032x", 9)
	if err := store.UpsertRun(ctx, completed(runID, 10)); err != nil {
		t.Fatal(err)
	}

	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The old epoch stays live and projects the run's advance. In production the
	// intake marker would be acknowledged at this point and disappear.
	if err := store.UpsertRun(ctx, completed(runID, 11)); err != nil {
		t.Fatal(err)
	}

	ids, err := rebuild.CatchUpRunIDs(ctx)
	if err != nil {
		t.Fatalf("catch-up ids: %v", err)
	}
	var found bool
	for _, id := range ids {
		if id == runID {
			found = true
		}
	}
	if !found {
		t.Errorf("the run that advanced during the rebuild is not in the catch-up set %v; "+
			"with its intake marker already acknowledged by the live epoch, the new epoch "+
			"would publish stale at source position 10 and nothing would report it", ids)
	}
	_ = rebuild.Abort()
}

// TestAbortLeavesNoArtefacts pins that a discarded build cleans up, including
// its WAL sidecars.
//
// Leaving a -wal beside a removed main file makes the next Open of that path
// recover a database out of the orphaned log — which is how an aborted rebuild's
// rows would reappear inside the next one.
func TestAbortLeavesNoArtefacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Target().UpsertRun(ctx, completed(fmt.Sprintf("%032x", 1), 1)); err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(rebuild.Path + suffix); !os.IsNotExist(err) {
			t.Errorf("abort left %s%s behind", filepath.Base(rebuild.Path), suffix)
		}
	}
}

// TestStartupDiscardsAnOrphanedRebuild pins the recovery path.
//
// §6.5 requires the change-retention pin to release on EVERY terminal outcome —
// success, abort, discard, and an orphan found at startup. Without the last one,
// a rebuild killed mid-flight blocks change pruning indefinitely and the feed
// grows without bound for a reason nobody is looking at.
func TestStartupDiscardsAnOrphanedRebuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatal(err)
	}

	rebuild, err := store.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a kill: the process dies without aborting or swapping.
	_ = rebuild.Target().Close()
	_ = store.Close()

	stale, err := StaleRebuildFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("startup found %d orphaned rebuild files, want 1: %v", len(stale), stale)
	}
	discarded, err := DiscardStaleRebuilds(dir)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 1 {
		t.Errorf("discarded %d orphans, want 1", discarded)
	}
	if remaining, _ := StaleRebuildFiles(dir); len(remaining) != 0 {
		t.Errorf("orphans survive after discard: %v", remaining)
	}

	// The live store is unaffected and reopens normally.
	reopened, err := Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatalf("the live store is unopenable after discarding an orphan: %v", err)
	}
	_ = reopened.Close()
}

// TestStaleScanDoesNotMatchTheLiveStore pins that the orphan scan cannot eat the
// database it is protecting.
//
// The live file is read.db; builds are read-<epoch>.db. A prefix match that was
// one character sloppier would discard the live store at every startup.
func TestStaleScanDoesNotMatchTheLiveStore(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stale, err := StaleRebuildFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range stale {
		if filepath.Base(file) == FileName {
			t.Fatalf("the orphan scan matched the LIVE store %s; startup recovery would "+
				"delete the read model on every boot", file)
		}
	}
}
