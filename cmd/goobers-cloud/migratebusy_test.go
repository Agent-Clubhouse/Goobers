package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

// TestMigrateRetriesBusyThenExitsTempFail: the migrate Job has no retry of its
// own, so a busy database is retried with backoff for the role's budget; a
// database that stays busy exits 75 (EX_TEMPFAIL), distinct from a real
// failure, and names re-sync as the remedy.
func TestMigrateRetriesBusyThenExitsTempFail(t *testing.T) {
	originalPools, originalRun, originalSignal := newCloudPools, runCloudMigrations, signalContext
	budget, backoff, backoffMax := migrateBusyBudget, migrateBusyBackoff, migrateBusyBackoffMax
	t.Cleanup(func() {
		newCloudPools, runCloudMigrations, signalContext = originalPools, originalRun, originalSignal
		migrateBusyBudget, migrateBusyBackoff, migrateBusyBackoffMax = budget, backoff, backoffMax
	})
	t.Setenv("GOOBERS_DATABASE_DIRECT_URL", "postgres://localhost/goobers")
	t.Setenv("GOOBERS_INSTANCE_ID", "test-instance")
	t.Setenv("GOOBERS_MIGRATION_BACKUP_REF", "pitr:test")
	signalContext = func() (context.Context, func()) { return context.Background(), func() {} }
	newCloudPools = func(context.Context, cloudpg.Config) (*cloudpg.Pools, error) { return &cloudpg.Pools{}, nil }
	migrateBusyBudget, migrateBusyBackoff, migrateBusyBackoffMax = time.Minute, time.Millisecond, 4*time.Millisecond
	busy := fmt.Errorf("%w: cloud migration 25 could not take its locks", cloudmigrate.ErrMigrationBusy)

	calls := 0
	runCloudMigrations = func(context.Context, *cloudpg.Pools, string, string, string, func(error)) error {
		calls++
		if calls < 3 {
			return busy
		}
		return nil
	}
	if code, stdout, stderr := runForTest("migrate", "apply"); code != 0 || calls != 3 ||
		strings.Count(stderr, "database busy, retrying") != 2 || !strings.Contains(stdout, "cloud migrations applied") {
		t.Fatalf("busy then clear: code=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
	}

	calls = 0
	migrateBusyBudget = 50 * time.Millisecond
	runCloudMigrations = func(context.Context, *cloudpg.Pools, string, string, string, func(error)) error {
		calls++
		return busy
	}
	code, _, stderr := runForTest("migrate", "apply")
	// The retry stops once the next backoff would overrun the budget, so the
	// message reports the time actually spent, not the configured budget.
	if code != exitTempFail || calls < 2 || !strings.Contains(stderr, "database still busy after 0s") || !strings.Contains(stderr, "re-sync the deployment") {
		t.Fatalf("persistently busy: code=%d calls=%d stderr=%q, want exit %d after retries", code, calls, stderr, exitTempFail)
	}

	// SIGTERM during the backoff stops the retry; the error keeps both causes
	// and the message does not claim the database stayed busy for the budget.
	calls = 0
	migrateBusyBudget, migrateBusyBackoff = time.Minute, time.Minute
	signalled, terminate := context.WithCancel(context.Background())
	signalContext = func() (context.Context, func()) { return signalled, terminate }
	runCloudMigrations = func(context.Context, *cloudpg.Pools, string, string, string, func(error)) error {
		calls++
		terminate()
		return busy
	}
	code, _, stderr = runForTest("migrate", "apply")
	if code != exitTempFail || calls != 1 || !strings.Contains(stderr, "stopped after 0s while the database was busy") || strings.Contains(stderr, "still busy") {
		t.Fatalf("terminated while busy: code=%d calls=%d stderr=%q", code, calls, stderr)
	}
	signalContext = func() (context.Context, func()) { return context.Background(), func() {} }

	calls = 0
	runCloudMigrations = func(context.Context, *cloudpg.Pools, string, string, string, func(error)) error {
		calls++
		return errors.New("syntax error in migration")
	}
	if code, _, _ := runForTest("migrate", "apply"); code != 1 || calls != 1 {
		t.Fatalf("real failure: code=%d calls=%d, want exit 1 with no retry", code, calls)
	}
}

func TestRetryMigrationsWhileBusyKeepsTheSignalOnTermination(t *testing.T) {
	backoff := migrateBusyBackoff
	t.Cleanup(func() { migrateBusyBackoff = backoff })
	migrateBusyBackoff = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	busy := fmt.Errorf("%w: locks", cloudmigrate.ErrMigrationBusy)
	err := retryMigrationsWhileBusy(ctx, &strings.Builder{}, func() error { cancel(); return busy })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cloudmigrate.ErrMigrationBusy) {
		t.Fatalf("terminated retry = %v, want both the cancellation and the busy cause", err)
	}
}
