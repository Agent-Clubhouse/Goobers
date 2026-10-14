package apireadstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func generation(t *testing.T, s *Store, snapshot string) string {
	t.Helper()
	value, err := s.SnapshotGeneration(snapshot)
	if err != nil || value == "" {
		t.Fatalf("generation=%q err=%v", value, err)
	}
	return value
}

func TestSnapshotGenerationsRotateIsolateAndBoundEviction(t *testing.T) {
	dir := t.TempDir()
	a, b := openTest(t, dir, 2, 1024), openTest(t, dir, 2, 1024)
	first := generation(t, a, "first")
	peer := generation(t, b, "other")
	if generation(t, b, "first") != first {
		t.Fatal("independent reader disagrees")
	}
	if err := b.InvalidateSnapshot("first"); err != nil {
		t.Fatal(err)
	}
	if generation(t, a, "first") == first {
		t.Fatal("invalidation did not fence earlier generation")
	}
	if generation(t, a, "other") != peer {
		t.Fatal("unrelated snapshot invalidated")
	}
	for i := range 10 {
		generation(t, a, fmt.Sprint(i))
	}
	var count int
	if err := a.db.QueryRow(`SELECT count(*) FROM snapshot_generations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unbounded metadata: %d %v", count, err)
	}
	if generation(t, b, "other") == peer {
		t.Fatal("evicted generation revived")
	}
}

func TestSnapshotGenerationMigrationPreservesV1Responses(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteschema.Migrate(t.Context(), db, "apireadstore", []string{schema}); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db, maxEntries: 8, maxBytes: 4096}
	if err := old.Put(entry("base", "old response", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	current := openTest(t, dir, 8, 4096)
	if saved, ok, err := current.Get("base", time.Unix(2, 0)); err != nil || !ok || string(saved.Body) != "old response" {
		t.Fatalf("v1 cache lost: %+v %v %v", saved, ok, err)
	}
	first := generation(t, current, "snapshot")
	peer := openTest(t, dir, 8, 4096)
	if generation(t, peer, "snapshot") != first {
		t.Fatal("reopen changed generation")
	}
}
