package rollup

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReaderPoolAllowsConcurrentReads pins the property #1916 exists for.
//
// Before it, one connection served every reader AND the writer, so concurrent
// reads gained nothing from each other. Measured on this fixture before the
// change: eight concurrent aggregate queries took 48.62ms against 48.37ms
// serial — a 0.99x "speedup", i.e. perfect serialization. That is design §5.2's
// "every reader serializes behind every reader *and* the writer, so the
// Overview's five concurrent requests have their queries serialized and an
// analytics aggregate blocks every list".
//
// The assertion is deliberately loose. A speedup ratio is a wall-clock
// measurement and this suite runs on contended, three-way-sharded CI runners, so
// it asserts only that concurrency is no longer *pathologically* absent —
// meaningfully better than 1.0x — rather than a specific multiple. A tighter
// bound would flake on exactly the hardware the rest of this wave learned not to
// time.
func TestReaderPoolAllowsConcurrentReads(t *testing.T) {
	db := seedConcurrencyFixture(t)

	query := func() error {
		row := db.readDB().QueryRow(
			`SELECT COUNT(*), MIN(started_at), MAX(started_at) FROM runs WHERE status = 'completed'`)
		var n int
		var lo, hi string
		return row.Scan(&n, &lo, &hi)
	}

	const n = 8
	start := time.Now()
	for i := 0; i < n; i++ {
		if err := query(); err != nil {
			t.Fatalf("serial query %d: %v", i, err)
		}
	}
	serial := time.Since(start)

	start = time.Now()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = query() }(i)
	}
	wg.Wait()
	concurrent := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent query %d: %v", i, err)
		}
	}

	speedup := float64(serial) / float64(concurrent)
	t.Logf("%d queries: serial=%s concurrent=%s speedup=%.2fx (pool size %d)",
		n, serial, concurrent, speedup, readerPoolSize())

	if db.reader == nil {
		t.Skip("no read-only pool on this platform; the single-handle fallback is in effect")
	}
	if speedup <= minConcurrencySpeedup {
		t.Errorf("concurrent reads gained nothing (speedup %.2fx <= %.2fx): reads are still serialized.\n"+
			"Before #1916 this measured 0.99x, which is the shape a single shared connection produces.",
			speedup, minConcurrencySpeedup)
	}
}

// minConcurrencySpeedup is the floor for "concurrency is doing something". Set
// just above 1.0 rather than near the core count, because the ratio is wall
// clock and CI runners are contended.
const minConcurrencySpeedup = 1.3

// TestReaderPoolIsReadOnly pins that the read handle structurally cannot write.
//
// This is the driver-level half of §3.1's "reads never write" boundary: the
// service layer enforces it with types, and mode=ro enforces it for anything
// that reaches SQL directly.
func TestReaderPoolIsReadOnly(t *testing.T) {
	db := seedConcurrencyFixture(t)
	if db.reader == nil {
		t.Skip("no read-only pool on this platform")
	}
	_, err := db.reader.Exec(`DELETE FROM runs`)
	if err == nil {
		t.Fatal("the read pool executed a DELETE; mode=ro is not in effect")
	}
	t.Logf("read pool correctly refused a write: %v", err)

	// And the writer must still work, or the split has broken ingest.
	if _, err := db.sql.Exec(`UPDATE runs SET status = 'completed' WHERE run_id = ?`, fmt.Sprintf("%032x", 1)); err != nil {
		t.Fatalf("writer handle rejected a write: %v", err)
	}
}

// TestFileURIHandlesPlatformPaths pins the URI construction the read-only pool
// depends on. mode=ro requires a file: URI, whereas the writer deliberately
// keeps a literal path to avoid URI-encoding pitfalls, so this is the one place
// path handling changes shape.
func TestFileURIHandlesPlatformPaths(t *testing.T) {
	if got := fileURI(""); got != "" {
		t.Errorf("fileURI(\"\") = %q, want empty", got)
	}
	// A path already carrying query syntax cannot be safely appended to.
	if got := fileURI("/tmp/db?foo=1"); got != "" {
		t.Errorf("fileURI with query syntax = %q, want empty", got)
	}
	uri := fileURI(filepath.Join(t.TempDir(), "telemetry.db"))
	if uri == "" {
		t.Fatal("fileURI returned empty for an ordinary path")
	}
	if len(uri) < len("file://") || uri[:len("file://")] != "file://" {
		t.Errorf("fileURI = %q, want a file:// URI", uri)
	}
	// Spaces must be escaped, or the DSN is truncated at the space.
	spaced := fileURI(filepath.Join(t.TempDir(), "my instance", "telemetry.db"))
	if spaced != "" && contains(spaced, " ") {
		t.Errorf("fileURI left an unescaped space: %q", spaced)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// seedConcurrencyFixture builds a rollup with enough rows that an aggregate is
// measurable rather than instant.
func seedConcurrencyFixture(t *testing.T) *DB {
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
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40000; i++ {
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

// TestCompactSucceedsWithReaderPoolOpen pins the one operation where the reader
// pool is not transparent.
//
// VACUUM rewrites the whole database and needs exclusive access. A read-only
// pool that is merely IDLE still holds file handles, so without releasing it
// first, compaction fails with "database is locked" — intermittently, depending
// on whether any read had ever happened. Compact releases the pool and readDB
// reopens it lazily, so compaction costs one reopen rather than permanently
// demoting the process to a single handle.
func TestCompactSucceedsWithReaderPoolOpen(t *testing.T) {
	db := seedConcurrencyFixture(t)

	// Force the pool into existence and make it hold connections.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row := db.readDB().QueryRow(`SELECT COUNT(*) FROM runs`)
			var n int
			_ = row.Scan(&n)
		}()
	}
	wg.Wait()
	if db.reader == nil {
		t.Skip("no read-only pool on this platform")
	}

	if err := db.Compact(context.Background()); err != nil {
		t.Fatalf("Compact failed with a reader pool open: %v", err)
	}

	// And reads must still work afterwards, via a lazily reopened pool.
	row := db.readDB().QueryRow(`SELECT COUNT(*) FROM runs`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("read after compact: %v", err)
	}
	if n != 40000 {
		t.Errorf("read %d rows after compact, want 40000", n)
	}
	if db.reader == nil {
		t.Error("reader pool was not reopened after compaction; the process is demoted to one handle")
	}
}
