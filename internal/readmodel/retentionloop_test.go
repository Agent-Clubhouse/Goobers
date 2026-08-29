package readmodel

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestUnboundedProjectionPassStillPrunesChangeFeed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 11; i++ {
		if err := store.PublishDefinitionsChanged(ctx); err != nil {
			t.Fatal(err)
		}
	}
	loop := NewRetentionLoop(store, store, UnboundedRetention(), RetentionOptions{
		ChangeFeedKeep: 10,
	})

	loop.pass(ctx)

	changes, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 10 {
		t.Fatalf("change rows after pass = %d, want 10", len(changes))
	}
	if loop.Stats().ChangesPruned != 1 {
		t.Errorf("changes pruned = %d, want 1", loop.Stats().ChangesPruned)
	}
}

// TestBoundedLoopRunsAPassOnStart pins that an instance down past its window
// catches up immediately rather than after a full interval.
//
// Run calls pass() once before entering its ticker loop; this asserts what that
// pass does. Asserting it through the goroutine instead means racing a timer,
// which is how the first version of this test came to fail only under load.
func TestBoundedLoopRunsAPassOnStart(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	seedAged(t, store, 1, now.AddDate(0, 0, -200))

	// Driven synchronously rather than by racing Run's goroutine against a
	// polling deadline. The property under test is "a pass happens before the
	// first tick", which pass() demonstrates directly; the goroutine version
	// was timing-dependent and failed under parallel load in the full suite
	// while passing in isolation.
	loop := NewRetentionLoop(store, store, RetentionDays(90), RetentionOptions{Interval: time.Hour})
	loop.pass(context.Background())

	if loop.Stats().Passes == 0 {
		t.Fatal("no pass ran; an instance down past its window would wait a full interval " +
			"before catching up")
	}
	if _, ok, _ := store.GetRun(context.Background(), fmt.Sprintf("%032x", 1)); ok {
		t.Error("a 200-day-old run survived a 90-day window after a pass")
	}
}

// TestPassFailureIsCountedNotFatal pins that housekeeping cannot take the daemon
// down.
//
// Retention operates on derived state: a failed pass leaves the projection
// larger than intended, which is a cost. Escalating that to a fatal would turn a
// cost into an outage.
func TestPassFailureIsCountedNotFatal(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	seedAged(t, store, 1, now.AddDate(0, 0, -200))

	loop := NewRetentionLoop(store, store, RetentionDays(90), RetentionOptions{})
	// Closing the store makes every query fail — the shutdown-overlap case.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Must not panic, and must record the failure.
	loop.pass(context.Background())

	if loop.Stats().Failures == 0 {
		t.Error("a failing pass was not counted; a retention loop silently doing nothing " +
			"looks identical to one with nothing to do")
	}
}

// TestCancelledPassIsNotCountedAsAFailure pins that shutdown is not an error.
//
// Every pass races daemon shutdown by construction. Counting cancellation as a
// failure would make the counter useless — it would be non-zero on every clean
// stop.
func TestCancelledPassIsNotCountedAsAFailure(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	seedAged(t, store, 1, now.AddDate(0, 0, -200))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loop := NewRetentionLoop(store, store, RetentionDays(90), RetentionOptions{})
	loop.pass(ctx)

	if loop.Stats().Failures != 0 {
		t.Errorf("a cancelled pass counted %d failures; the counter would be non-zero on "+
			"every clean shutdown", loop.Stats().Failures)
	}
}
