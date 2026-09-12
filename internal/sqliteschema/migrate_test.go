package sqliteschema

import (
	"database/sql"
	"path/filepath"
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
