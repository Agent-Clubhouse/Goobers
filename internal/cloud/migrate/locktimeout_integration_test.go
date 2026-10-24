//go:build cloudintegration

package migrate

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMigrationGivesUpOnALockAReaderHolds: a migration whose DDL needs a lock
// a long reader holds must not queue for it, because a queued ACCESS EXCLUSIVE
// request blocks every later append and read of the table. It gives up within
// its lock timeout, retries, fails as busy, and leaves no queued lock behind;
// once the reader finishes, the same migration applies.
func TestMigrationGivesUpOnALockAReaderHolds(t *testing.T) {
	pool := privatePool(t)
	ensureRoles(t, pool)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "migration", Holder: "test"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	timeout, tries, backoff := migrationLockTimeout, migrationLockTries, migrationLockBackoff
	migrationLockTimeout, migrationLockTries, migrationLockBackoff = 200*time.Millisecond, 2, 50*time.Millisecond
	t.Cleanup(func() { migrationLockTimeout, migrationLockTries, migrationLockBackoff = timeout, tries, backoff })

	reader, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := reader.Exec(t.Context(), `SELECT count(*) FROM journal.run_event`); err != nil {
		t.Fatal(err)
	}
	probe := Migration{
		Version: catalog[len(catalog)-1].Version + 1000, Kind: Expand, Name: "lock_probe",
		BinaryVersion: "0.7.0", MinCompatibleBinary: "0.6.0",
		SQL: `LOCK TABLE journal.run_event IN ACCESS EXCLUSIVE MODE`,
	}
	runner.Catalog = append(append([]Migration(nil), catalog...), probe)
	// A deadline far beyond the lock budget: a queued lock would run into it
	// instead of failing as busy.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := runner.Apply(ctx); !errors.Is(err, ErrMigrationBusy) || !isLockTimeout(err) {
		t.Fatalf("apply against a held reader = %v, want busy from the lock timeout", err)
	}
	var waiting int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_locks
		WHERE NOT granted AND relation='journal.run_event'::regclass`).Scan(&waiting); err != nil || waiting != 0 {
		t.Fatalf("queued locks on journal.run_event = %d (%v), want none", waiting, err)
	}
	if err := reader.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("apply after the reader finished: %v", err)
	}
}
