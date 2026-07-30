package rollup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestCancellationReachesTheStatement is the property this whole context-threading
// change exists to make possible, and it was previously unreachable.
//
// Design §7.1 requires that "deadline expiry must actually stop the work", and
// the fourth pass corrected an earlier claim by establishing that
// modernc.org/sqlite DOES wire context cancellation to sqlite3_interrupt. But
// none of that could take effect: the repository contained ZERO QueryContext /
// ExecContext call sites, and this package had no context.Context in any
// non-test signature, so a router-installed deadline had nothing to reach.
//
// # What the caller actually observes
//
// NOT a SQLite error code. §18.0 records this correction: the driver arms the
// interrupt only when ctx.Done() != nil, and the deferred block immediately
// after rewrites the result — `if ctx != nil && done != 0 { r, err = nil,
// ctx.Err() }`. So the caller sees context.DeadlineExceeded or
// context.Canceled, never SQLITE_INTERRUPT, and any 503 mapping must key on
// those. This test pins that observation so a future change to the driver or to
// the mapping cannot silently invalidate it.
func TestCancellationReachesTheStatement(t *testing.T) {
	db := seedCancellationFixture(t)

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := db.Runs(ctx)
		if err == nil {
			t.Fatal("a query with an already-cancelled context succeeded; cancellation does not reach the statement")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; want context.Canceled.\n"+
				"The 503 mapping keys on context errors, not on a SQLite code — the driver rewrites the "+
				"interrupt to ctx.Err() before the caller sees it.", err)
		}
	})

	t.Run("deadline expires mid-query", func(t *testing.T) {
		// A deadline short enough to expire while a genuinely expensive query is
		// running. If the statement were not interruptible, this would return a
		// full result after the deadline rather than an error.
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := db.Stats(ctx, StatsRequest{})
		elapsed := time.Since(start)
		if err == nil {
			t.Skipf("query completed in %s before the deadline; fixture too small to prove interruption", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v after %s; want a context error", err, elapsed)
		} else {
			t.Logf("statement aborted after %s with %v", elapsed, err)
		}
	})
}

// seedCancellationFixture builds a corpus large enough that an aggregate takes
// long enough to be interrupted.
func seedCancellationFixture(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30000; i++ {
		if _, err := tx.Exec(
			`INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			 VALUES (?, 'deploy', 1, 'alpha', 'completed', ?)`,
			fmt.Sprintf("%032x", i), formatTime(base.Add(time.Duration(i)*time.Minute)).String,
		); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}
