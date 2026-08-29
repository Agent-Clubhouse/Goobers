package readmodel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func seedAged(t *testing.T, store *Store, n int, startedAt time.Time) string {
	t.Helper()
	runID := fmt.Sprintf("%032x", n)
	finished := startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), Projection{Run: RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: uint64(n + 1),
	}}); err != nil {
		t.Fatalf("seed %d: %v", n, err)
	}
	return runID
}

// TestUnboundedRetentionAgesOutNothing is the most important test in this file.
//
// The obvious encoding of "off" — a day count of 0 — is actively dangerous:
// compared naively, `startedAt < now - 0 days` ages out EVERY run immediately.
// That is the most destructive possible reading of the value an operator would
// most reasonably expect to mean "leave it alone", and it would silently delete
// the entire projection.
//
// Zero, negative, and unset must all be unbounded, and must reach that answer
// WITHOUT touching the floor arithmetic.
func TestUnboundedRetentionAgesOutNothing(t *testing.T) {
	ctx := context.Background()

	for _, days := range []int{0, -1, -90} {
		window := RetentionDays(days)
		if window.Bounded() {
			t.Fatalf("RetentionDays(%d) is bounded; it must be unbounded", days)
		}
		if !window.FloorAt(time.Now()).IsZero() {
			t.Errorf("RetentionDays(%d) produced a non-zero floor", days)
		}
	}

	store := openTestStore(t)
	ancient := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	runID := seedAged(t, store, 1, ancient)

	result, err := store.ApplyRetention(ctx, store, UnboundedRetention(), 100)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.AgedOut != 0 || result.Tombstoned != 0 {
		t.Errorf("unbounded retention aged out %d runs and tombstoned %d; it must do neither",
			result.AgedOut, result.Tombstoned)
	}
	if _, ok, _ := store.GetRun(ctx, runID); !ok {
		t.Error("a six-year-old run was deleted under UNBOUNDED retention — the exact " +
			"failure a zero-as-off encoding produces")
	}
	if _, ok, _ := store.ProjectionFloor(ctx); ok {
		t.Error("unbounded retention advanced the projection floor")
	}
}

// TestBoundedRetentionAgesOutOnlyBelowTheFloor pins the window itself.
func TestBoundedRetentionAgesOutOnlyBelowTheFloor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	old := seedAged(t, store, 1, now.AddDate(0, 0, -120))
	edge := seedAged(t, store, 2, now.AddDate(0, 0, -89))
	fresh := seedAged(t, store, 3, now.AddDate(0, 0, -1))

	result, err := store.ApplyRetention(ctx, store, RetentionDays(90), 100)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.AgedOut != 1 {
		t.Errorf("aged out %d runs, want 1", result.AgedOut)
	}
	if _, ok, _ := store.GetRun(ctx, old); ok {
		t.Error("a 120-day-old run survived a 90-day window")
	}
	for _, kept := range []string{edge, fresh} {
		if _, ok, _ := store.GetRun(ctx, kept); !ok {
			t.Errorf("run %s inside the window was aged out", kept)
		}
	}
}

// TestRetentionTombstonesBeforeRemoving pins the ordering that prevents a
// livelock.
//
// A removal with no tombstone leaves a run the repair sweep re-admits from its
// journal, retention deletes again, and the cycle repeats — consuming the
// sweep's whole budget and flooding the change feed. The tombstone is what makes
// "deliberately aged out" distinguishable from "missing".
func TestRetentionTombstonesBeforeRemoving(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	runID := seedAged(t, store, 1, now.AddDate(0, 0, -200))
	if _, err := store.ApplyRetention(ctx, store, RetentionDays(90), 100); err != nil {
		t.Fatalf("apply: %v", err)
	}

	tombstoned, err := store.Tombstoned(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Error("an aged-out run was removed without a tombstone; repair will re-admit it " +
			"from its journal and retention will delete it again, forever")
	}
	if _, ok, _ := store.GetRun(ctx, runID); ok {
		t.Error("the run was tombstoned but not removed")
	}
}

// TestRetentionAdvancesTheFloorLast pins the other half of the ordering.
//
// If the floor advanced first and the pass then failed, the floor would exclude
// runs that are still present, and repair would skip them forever as though they
// had been aged out — leaving rows nothing will ever reconcile.
func TestRetentionAdvancesTheFloorLast(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	seedAged(t, store, 1, now.AddDate(0, 0, -200))

	if _, ok, _ := store.ProjectionFloor(ctx); ok {
		t.Fatal("a fresh store already has a floor")
	}
	if _, err := store.ApplyRetention(ctx, store, RetentionDays(90), 100); err != nil {
		t.Fatal(err)
	}
	floor, ok, err := store.ProjectionFloor(ctx)
	if err != nil || !ok {
		t.Fatalf("floor after retention: ok=%v err=%v", ok, err)
	}
	if !floor.Equal(RetentionDays(90).FloorAt(now)) {
		t.Errorf("floor = %s, want %s", floor, RetentionDays(90).FloorAt(now))
	}
}

// TestRetentionIsBatched pins that one pass cannot monopolise the writer.
//
// Retention runs alongside serving. An unbounded pass would block every read
// behind it, and would emit one enormous burst of removals into the change feed.
func TestRetentionIsBatched(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	for i := 0; i < 25; i++ {
		seedAged(t, store, i, now.AddDate(0, 0, -200-i))
	}

	result, err := store.ApplyRetention(ctx, store, RetentionDays(90), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgedOut > 10 {
		t.Errorf("one pass aged out %d runs against a batch limit of 10", result.AgedOut)
	}
}

// TestPruneChangeFeedBoundsGrowth is #1919's transferred acceptance criterion.
//
// PruneChanges existed but nothing called it, so the change feed grew without
// bound — the mechanism was present and inert.
func TestPruneChangeFeedBoundsGrowth(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		seedAged(t, store, i, base.Add(time.Duration(i)*time.Minute))
	}

	before, err := store.Changes(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 40 {
		t.Fatalf("expected at least 40 changes, got %d", len(before))
	}

	removed, err := store.PruneChangeFeed(ctx, 10)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Error("pruning to a 10-row window removed nothing")
	}
	after, err := store.Changes(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) >= len(before) {
		t.Errorf("feed did not shrink: %d -> %d", len(before), len(after))
	}
	if len(after) != 10 {
		t.Errorf("feed retained %d rows, want exact 10-row bound", len(after))
	}
}

// TestPruneChangeFeedKeepsAShortFeedIntact pins that pruning a feed smaller than
// the window is a no-op rather than an error or a wipe.
func TestPruneChangeFeedKeepsAShortFeedIntact(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedAged(t, store, 1, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	removed, err := store.PruneChangeFeed(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("pruned %d rows from a feed shorter than the window", removed)
	}
}
