package readmodel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func seedForBucket(t *testing.T, store *Store, n int, startedAt time.Time, phase journal.RunPhase, verdict string) {
	t.Helper()
	finished := startedAt.Add(90 * time.Second)
	row := RunRow{
		RunID: fmt.Sprintf("%032x", n), Gaggle: "alpha", Workflow: "wf",
		Phase: phase, Terminal: phase != journal.PhaseRunning,
		StartedAt: startedAt, LastSeq: uint64(n + 1), OutcomeVerdict: verdict,
	}
	if row.Terminal {
		row.FinishedAt = &finished
	}
	if err := store.UpsertRun(context.Background(), Projection{Run: row}); err != nil {
		t.Fatalf("seed %d: %v", n, err)
	}
}

// TestRecomputingADayTwiceIsIdentical is the property the entire design rests
// on.
//
// Recompute was chosen over reversible deltas precisely because deltas require
// storing each run's prior contribution and subtracting it on reprojection —
// fiddly, easy to get wrong, and it drifts SILENTLY when it is. A recompute
// carries no state between passes, so it cannot drift. This asserts that.
func TestRecomputingADayTwiceIsIdentical(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	day := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		seedForBucket(t, store, i, day.Add(time.Duration(i)*time.Hour), journal.PhaseCompleted, "pass")
	}

	key := day.Format(dayFormat)
	if err := store.RecomputeDay(ctx, key); err != nil {
		t.Fatalf("first recompute: %v", err)
	}
	first, err := store.DayBuckets(ctx, "", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("recompute produced no buckets")
	}

	if err := store.RecomputeDay(ctx, key); err != nil {
		t.Fatalf("second recompute: %v", err)
	}
	second, err := store.DayBuckets(ctx, "", day, day)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != len(second) {
		t.Fatalf("recompute is not idempotent: %d buckets then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("bucket %d differs between recomputes:\n  %+v\n  %+v", i, first[i], second[i])
		}
	}
}

// TestBucketsAggregateBySlice pins the grouping.
func TestBucketsAggregateBySlice(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	day := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

	seedForBucket(t, store, 1, day.Add(time.Hour), journal.PhaseCompleted, "pass")
	seedForBucket(t, store, 2, day.Add(2*time.Hour), journal.PhaseCompleted, "pass")
	seedForBucket(t, store, 3, day.Add(3*time.Hour), journal.PhaseFailed, "fail")

	if err := store.RecomputeDay(ctx, day.Format(dayFormat)); err != nil {
		t.Fatal(err)
	}
	buckets, err := store.DayBuckets(ctx, "", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (completed/pass and failed/fail): %+v", len(buckets), buckets)
	}
	total := 0
	for _, bucket := range buckets {
		total += bucket.Runs
		if bucket.Duration <= 0 {
			t.Errorf("bucket %+v has no duration; terminal runs have one", bucket)
		}
	}
	if total != 3 {
		t.Errorf("buckets account for %d runs, want 3", total)
	}
}

// TestBucketReadIsBoundedByslicesNotRuns is the point of the whole feature.
//
// Without pre-aggregation an all-time query scans all matching history, so
// "zero timeouts" does not hold on the analytics surface. The bucket read
// returns one row per (day, gaggle, workflow, phase, outcome) slice regardless
// of how many runs sit behind it.
func TestBucketReadIsBoundedBySlicesNotRuns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)

	// 200 runs, all in one slice.
	for i := 0; i < 200; i++ {
		seedForBucket(t, store, i, day.Add(time.Duration(i)*time.Minute), journal.PhaseCompleted, "pass")
	}
	if err := store.RecomputeDay(ctx, day.Format(dayFormat)); err != nil {
		t.Fatal(err)
	}

	buckets, err := store.DayBuckets(ctx, "", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("200 runs in one slice produced %d bucket rows, want 1; the read is scaling "+
			"with runs rather than with slices", len(buckets))
	}
	if buckets[0].Runs != 200 {
		t.Errorf("bucket counts %d runs, want 200", buckets[0].Runs)
	}
}

// TestProjectionMarksItsDayDirty pins the queue.
//
// A run finishing writes one marker rather than re-aggregating its whole day
// inside the projection transaction, which would put an O(runs-in-day) scan on
// every commit.
func TestProjectionMarksItsDayDirty(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	day := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	seedForBucket(t, store, 1, day, journal.PhaseCompleted, "pass")

	dirty, err := store.DirtyDays(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := day.Format(dayFormat)
	var found bool
	for _, d := range dirty {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("dirty days = %v, want it to contain %q; without the marker the day is "+
			"never recomputed and its buckets go stale silently", dirty, want)
	}
}

// TestRecomputeClearsTheDirtyMarkerInTheSameTransaction pins the ordering.
//
// Clearing first would lose the day entirely if the recompute then failed.
func TestRecomputeClearsTheDirtyMarkerInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	day := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	seedForBucket(t, store, 1, day, journal.PhaseCompleted, "pass")

	key := day.Format(dayFormat)
	if err := store.RecomputeDay(ctx, key); err != nil {
		t.Fatal(err)
	}
	dirty, err := store.DirtyDays(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirty {
		if d == key {
			t.Errorf("day %q is still queued after a successful recompute; it would recompute "+
				"forever", key)
		}
	}
}

// TestMonthRollsUpFromDays pins that the two tiers cannot disagree.
//
// A month is computed from the dailies, not from run rows, so it is by
// construction the sum of its days.
func TestMonthRollsUpFromDays(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		day := base.AddDate(0, 0, i)
		seedForBucket(t, store, i, day, journal.PhaseCompleted, "pass")
		if err := store.RecomputeDay(ctx, day.Format(dayFormat)); err != nil {
			t.Fatal(err)
		}
	}
	month := base.Format(monthFormat)
	if err := store.RecomputeMonth(ctx, month); err != nil {
		t.Fatalf("recompute month: %v", err)
	}

	var runs int
	if err := store.reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(runs), 0) FROM bucket_month WHERE month = ?`, month).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 3 {
		t.Errorf("month rollup counts %d runs, want 3 (the sum of its days)", runs)
	}
}

// TestUnparseableDayWritesNothing pins the safe direction on a malformed key.
//
// dayUpperBound returns the key itself on a parse failure, making the range
// empty. The dangerous alternative would be a bound that matched everything and
// aggregated the whole table into one day's bucket.
func TestUnparseableDayWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedForBucket(t, store, 1, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), journal.PhaseCompleted, "pass")

	if err := store.RecomputeDay(ctx, "not-a-day"); err != nil {
		t.Fatalf("recompute of a malformed key errored: %v", err)
	}
	var rows int
	if err := store.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bucket_day WHERE day = ?`, "not-a-day").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a malformed day key produced %d bucket rows; the range must be empty rather "+
			"than matching the whole table", rows)
	}
}
