package readmodel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

// migrationPrefixDigest hashes an ordered slice of migration statements so a
// reordering or in-place edit of any one of them changes the result, even
// though nothing about the slice's length does.
func migrationPrefixDigest(prefix []string) string {
	h := sha256.New()
	for _, m := range prefix {
		h.Write([]byte(m))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestMigrationPrefixIsAppendOnly pins schema.go's "never edit a migration once
// released — append a new one" rule with something other than a comment
// (#2049), mirroring the same guard added to internal/telemetry/rollup. Only
// the newest migration is left out of the pinned prefix, so a legitimate
// append requires updating wantDigest — but reordering, editing, or inserting
// before it changes the digest of migrations that were never touched by this
// commit, which is exactly the class of mistake the comment alone cannot
// catch: every upgraded store silently stops applying the inserted DDL
// forever while fresh stores get it, the worst kind of schema divergence.
func TestMigrationPrefixIsAppendOnly(t *testing.T) {
	const wantDigest = "c6084eeac15438a72479cd892596fa4d558d2e539ef98e8e1fdbaaa0820a6a7d"
	if got := migrationPrefixDigest(migrations[:len(migrations)-1]); got != wantDigest {
		t.Fatalf("migration prefix digest = %s, want %s\n"+
			"migrations must be append-only. If this commit only APPENDED a new\n"+
			"migration to the end of the list, update wantDigest to the value\n"+
			"above. If it did anything else to an existing entry, that is the\n"+
			"bug #2049 exists to catch.", got, wantDigest)
	}
}

func TestSweepRootCursorMigrationPreservesActiveForwardPosition(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const perRootCursorMigration = 22
	for i, migration := range migrations[:perRootCursorMigration-1] {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			t.Fatalf("seed migration %d: %v", i+1, err)
		}
	}
	started := time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)
	if _, err := tx.ExecContext(ctx, `
		UPDATE sweep_cursor SET root = ?, after_name = ?, cycle_started_at = ?,
			entries_this_cycle = ? WHERE id = 1`,
		"/instance/gaggles/alpha/runs", "run-0042", formatTime(started), 42,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cursors, err := store.SweepRootCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 1 {
		t.Fatalf("root cursors = %d, want migrated active root", len(cursors))
	}
	got := cursors[0]
	if got.Root != "/instance/gaggles/alpha/runs" || got.AfterName != "run-0042" ||
		got.EntriesThisCycle != 42 || !got.CycleStartedAt.Equal(started) {
		t.Fatalf("migrated root cursor = %+v, want existing forward position preserved", got)
	}
}

func TestDispositionMigrationMarksExistingProjectionUnready(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const dispositionMigration = 21
	for i, migration := range migrations[:dispositionMigration-1] {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			t.Fatalf("seed migration %d: %v", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projection_state SET ready = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Ready {
		t.Fatal("disposition migration left the projection ready; historical terminal unknown rows would not be replayed")
	}
}

func TestStageOutcomeMigrationMarksProjectionUnreadyAndClearsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const stageOutcomeMigration = 8
	for i, migration := range migrations[:stageOutcomeMigration] {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			t.Fatalf("seed migration %d: %v", i+1, err)
		}
	}
	started := time.Now().UTC().Format(timeFormat)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run (run_id, gaggle, workflow, phase, terminal, started_at)
		VALUES ('old-run', 'example', 'workflow', 'completed', 1, ?)`, started); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_stage (run_id, stage, attempts, last_status)
		VALUES ('old-run', 'implement', 2, 'success')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Ready {
		t.Fatal("migrated projection is ready before its rows have been reconstructed")
	}
	page, err := store.ListRuns(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 0 {
		t.Fatalf("migration retained %d stale run rows, want none", len(page.Runs))
	}
}
