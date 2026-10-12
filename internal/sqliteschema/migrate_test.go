package sqliteschema

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/sqliteuri"

	_ "modernc.org/sqlite"
)

func TestConcurrentMigrationUsesOneVersionedTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_pragma=busy_timeout(5000)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	dbs := []*sql.DB{open(), open()}
	migrations := []string{`CREATE TABLE records (id INTEGER PRIMARY KEY)`}
	var wg sync.WaitGroup
	for _, db := range dbs {
		wg.Go(func() {
			if err := Migrate(t.Context(), db, "fixture", migrations); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count, version int
	if err := dbs[0].QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_meta`).Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 1 {
		t.Fatalf("schema_meta count=%d version=%d, want count=1 version=1", count, version)
	}
}

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRejectsInvalidSchemaMetaRowCount(t *testing.T) {
	for name, setup := range map[string][]string{
		"multiple rows": {
			`CREATE TABLE schema_meta (version INTEGER NOT NULL)`,
			`INSERT INTO schema_meta VALUES (1)`,
			`INSERT INTO schema_meta VALUES (1)`,
		},
		"existing empty table": {`CREATE TABLE schema_meta (version INTEGER NOT NULL)`},
	} {
		t.Run(name, func(t *testing.T) {
			db := openMem(t)
			for _, q := range setup {
				mustExec(t, db, q)
			}
			err := Migrate(t.Context(), db, "fixture", []string{`CREATE TABLE records (id INTEGER)`})
			if err == nil || !strings.Contains(err.Error(), "schema_meta contains") {
				t.Fatalf("err = %v, want row-count error", err)
			}
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'records'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("records table created despite refusal: n=%d err=%v", n, err)
			}
		})
	}
}

func TestMigrateRejectsNewerAndInvalidVersions(t *testing.T) {
	for name, tc := range map[string]struct {
		version int
		want    string
	}{
		"newer":    {5, "newer than this build supports"},
		"negative": {-1, "is invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			db := openMem(t)
			mustExec(t, db, `CREATE TABLE schema_meta (version INTEGER NOT NULL)`)
			if _, err := db.Exec(`INSERT INTO schema_meta VALUES (?)`, tc.version); err != nil {
				t.Fatal(err)
			}
			err := Migrate(t.Context(), db, "fixture", []string{`CREATE TABLE records (id INTEGER)`})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			var v int
			if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&v); err != nil || v != tc.version {
				t.Fatalf("version = %d err=%v, want unchanged %d", v, err, tc.version)
			}
		})
	}
}

func TestMigrateFailingMigrationRollsBackAtomically(t *testing.T) {
	db := openMem(t)
	err := Migrate(t.Context(), db, "fixture", []string{
		`CREATE TABLE records (id INTEGER)`,
		`NOT VALID SQL`,
	})
	if err == nil || !strings.Contains(err.Error(), "apply schema migration 2") {
		t.Fatalf("err = %v, want migration 2 failure", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('records', 'schema_meta')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration persisted: n=%d err=%v", n, err)
	}
}

func TestMigrateAppliesOnlyPendingMigrations(t *testing.T) {
	db := openMem(t)
	first := []string{`CREATE TABLE a (id INTEGER)`}
	if err := Migrate(t.Context(), db, "fixture", first); err != nil {
		t.Fatal(err)
	}
	// Re-running migration 1 would fail because table a already exists.
	if err := Migrate(t.Context(), db, "fixture", append(first, `CREATE TABLE b (id INTEGER)`)); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("version = %d err=%v, want 2", v, err)
	}
}

func TestMigrateReturnsBeginError(t *testing.T) {
	db := openMem(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	err := Migrate(t.Context(), db, "fixture", nil)
	if err == nil || !strings.Contains(err.Error(), "begin schema migration") {
		t.Fatalf("err = %v, want begin error", err)
	}
}
