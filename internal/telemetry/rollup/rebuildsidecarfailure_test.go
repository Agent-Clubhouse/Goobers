package rollup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openActiveDBWithLiveWAL opens a real SQLite database in WAL mode, disables
// auto-checkpointing, and commits a row — leaving a genuine non-empty -wal
// sidecar on disk that has not been folded back into the main file, the way a
// best-effort checkpointWAL can leave the active telemetry.db between ingests.
// The returned handle is kept open (and closed via t.Cleanup) because SQLite
// checkpoints the WAL automatically when the last connection to a database
// closes, which would erase the very condition this test needs to hold.
func openActiveDBWithLiveWAL(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatalf("open active db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE marker (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO marker (value) VALUES ('active-projection')`); err != nil {
		t.Fatalf("insert marker row: %v", err)
	}
	return db
}

func readMarkerValue(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen active db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRow(`SELECT value FROM marker LIMIT 1`).Scan(&value); err != nil {
		t.Fatalf("read marker row: %v", err)
	}
	return value
}

// TestReplaceDBFailurePreservesActiveDatabaseAndSidecars is the merge-review
// finding on PR #3850: replaceDB used to delete the active database's -wal,
// -shm, and -journal sidecars before calling durability.ReplaceFile. If the
// swap then failed (a sharing/permission error, or a failed sidecar removal),
// the old main database file was left behind with its sidecars gone — and any
// committed pages that were still sitting in the deleted WAL, never
// checkpointed into the main file, were lost even though the "replacement
// failed" error told the caller the previous projection was untouched.
//
// replaceDB now stages sidecars out of the way with a reversible rename and
// only discards them once durability.ReplaceFile has actually committed the
// swap, restoring them on any failure. This proves the fixed swap: an active,
// WAL-backed database survives a failed replacement with all of its sidecars
// and its WAL-only committed data intact and queryable.
func TestReplaceDBFailurePreservesActiveDatabaseAndSidecars(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "telemetry.db")

	openActiveDBWithLiveWAL(t, dbPath)

	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat %s: %v", walPath, err)
	}
	if walInfo.Size() == 0 {
		t.Fatalf("-wal sidecar at %s is empty; test setup did not produce a live WAL", walPath)
	}
	walBefore, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read %s: %v", walPath, err)
	}
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read %s: %v", dbPath, err)
	}

	// stagingPath deliberately does not exist, so durability.ReplaceFile fails
	// partway through the swap — the same shape of failure (a sharing or
	// permission error on the rename) the merge-review finding described.
	stagingPath := filepath.Join(tmp, ".telemetry.db.rebuild-missing")
	if err := replaceDB(dbPath, stagingPath); err == nil {
		t.Fatal("replaceDB: want an error when the staged rollup is missing, got nil")
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("active database missing after failed replaceDB: %v", err)
	}
	dbAfter, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read %s after failed replaceDB: %v", dbPath, err)
	}
	if string(dbAfter) != string(dbBefore) {
		t.Fatalf("active database content changed after a failed replaceDB")
	}

	walAfter, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("-wal sidecar missing after failed replaceDB: %v", err)
	}
	if string(walAfter) != string(walBefore) {
		t.Fatalf("-wal sidecar content changed after a failed replaceDB")
	}

	if got := readMarkerValue(t, dbPath); got != "active-projection" {
		t.Fatalf("marker row after failed replaceDB = %q, want the WAL-backed row still readable", got)
	}

	// No trash files should be left dangling around after a failed swap either.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read %s: %v", tmp, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".rebuild-trash" {
			t.Fatalf("leftover trashed sidecar after failed replaceDB: %s", e.Name())
		}
	}
}

// TestReplaceDBSuccessDiscardsOldSidecars is the other side of the same
// fix: once durability.ReplaceFile actually commits the swap, the old
// sidecars that were staged out of the way must be permanently discarded, not
// left behind next to the newly active database.
func TestReplaceDBSuccessDiscardsOldSidecars(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "telemetry.db")

	openActiveDBWithLiveWAL(t, dbPath)
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("stat %s: %v", dbPath+"-wal", err)
	}

	stagingPath, err := createStagingDB(dbPath)
	if err != nil {
		t.Fatalf("createStagingDB: %v", err)
	}
	if err := os.WriteFile(stagingPath, []byte("staged-projection"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}

	if err := replaceDB(dbPath, stagingPath); err != nil {
		t.Fatalf("replaceDB: %v", err)
	}

	body, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read %s: %v", dbPath, err)
	}
	if string(body) != "staged-projection" {
		t.Fatalf("active database content = %q, want the staged projection", body)
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("-wal sidecar still present after a successful replaceDB: err=%v", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read %s: %v", tmp, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".rebuild-trash" {
			t.Fatalf("leftover trashed sidecar after a successful replaceDB: %s", e.Name())
		}
	}
}
