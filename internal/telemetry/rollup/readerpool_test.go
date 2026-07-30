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
// # Why this asserts structure and merely LOGS the speedup
//
// The first version asserted a speedup ratio above 1.3x, on the reasoning that
// it was "deliberately loose". It flaked on CI at 1.25x with a pool size of 3 —
// a three-core runner, contended, under -race. That is the same lesson this wave
// has now learned three times: a wall-clock ratio cannot be defended on shared
// hardware, and loosening the threshold only moves the flake rather than
// removing it.
//
// So the assertion is the STRUCTURAL property — a read-only pool exists and can
// open more than one connection — which is deterministic on any machine. The
// speedup is measured and logged, because it is the reason the structure matters
// and a reviewer should see it, but it does not gate the suite.
//
// The measurement that justified the change stands on its own: on the reference
// host, eight concurrent aggregates went from 48.62ms (0.99x, perfect
// serialization) to 13.48ms (3.54x).
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

	// The deterministic assertion: reads go to a pool that can actually run more
	// than one at a time. Before #1916 every read shared the writer's single
	// connection, so this was structurally 1.
	stats := db.reader.Stats()
	if stats.MaxOpenConnections < 2 {
		t.Errorf("read pool allows %d concurrent connections; reads are still serialized",
			stats.MaxOpenConnections)
	}
	if speedup <= 1.0 {
		// Not a failure — a contended runner can legitimately produce this — but
		// worth surfacing, since a persistent sub-1.0 across runs would mean the
		// pool is not being used at all.
		t.Logf("note: no speedup observed (%.2fx) on a %d-connection pool; expected on a contended or low-core host",
			speedup, stats.MaxOpenConnections)
	}
}

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
