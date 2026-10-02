package apireadstore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func openTest(t *testing.T, dir string, count, size int) *Store {
	t.Helper()
	s, err := Open(dir, count, size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func entry(key, body string, stored int64, snapshot string) Entry {
	return Entry{Key: key, Body: []byte(body), Metadata: []byte(`{"etag":"strong"}`), Stored: stored, Expires: stored + 100, Snapshot: snapshot}
}
func assertTotals(t *testing.T, s *Store, wantCount, wantBytes int) {
	t.Helper()
	var count, size int
	if err := s.db.QueryRow("SELECT entries,bytes FROM totals").Scan(&count, &size); err != nil {
		t.Fatal(err)
	}
	if count != wantCount || size != wantBytes {
		t.Fatalf("totals=%d/%d want %d/%d", count, size, wantCount, wantBytes)
	}
	var actualCount, actualBytes int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM entries),coalesce((SELECT sum(length(body)) FROM bodies),0)").Scan(&actualCount, &actualBytes); err != nil {
		t.Fatal(err)
	}
	if count != actualCount || size != actualBytes {
		t.Fatalf("counter drift: %d/%d vs %d/%d", count, size, actualCount, actualBytes)
	}
}
func TestSharedBodiesReplacementAndInvalidation(t *testing.T) {
	s := openTest(t, t.TempDir(), 512, 1024)
	if err := s.Put(entry("base", "one", 1, ""), entry("alias", "one", 1, "tick")); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, s, 2, 3)
	if err := s.Put(entry("base", "two", 2, "")); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, s, 2, 6)
	old, ok, err := s.Get("alias", time.Unix(2, 0))
	if err != nil || !ok || string(old.Body) != "one" {
		t.Fatalf("snapshot=%+v %v %v", old, ok, err)
	}
	if err := s.InvalidateSnapshot("tick"); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, s, 1, 3)
	if err := s.InvalidateSnapshot("tick"); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, s, 1, 3)
}
func TestEvictionBoundsUniqueBytesAndPreservesBaseBeforeAlias(t *testing.T) {
	s := openTest(t, t.TempDir(), 10, 7)
	if err := s.Put(entry("base-b", "6789", 1, ""), entry("base-a", "12345", 2, ""), entry("snapshot-a", "12345", 2, "tick")); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, s, 2, 5)
	if _, ok, _ := s.Get("base-b", time.Unix(2, 0)); ok {
		t.Fatal("old body retained over byte cap")
	}
	s.maxEntries = 1
	if err := s.Put(entry("snapshot-a", "12345", 2, "tick")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("base-a", time.Unix(2, 0)); !ok {
		t.Fatal("base evicted before same-age snapshot")
	}
	assertTotals(t, s, 1, 5)
}
func TestPeerWritesExpiryCorruptionAndOversizedReplacement(t *testing.T) {
	dir := t.TempDir()
	a := openTest(t, dir, 10, 7)
	b := openTest(t, dir, 10, 7)
	if err := a.Put(entry("a", "123", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(entry("b", "456", 2, "")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.Get("b", time.Unix(2, 0)); err != nil || !ok {
		t.Fatalf("peer write missing: %v", err)
	}
	if _, ok, _ := a.Get("a", time.Unix(102, 0)); ok {
		t.Fatal("expired response served")
	}
	if err := a.Put(entry("a", "too-large", 3, "")); err != nil {
		t.Fatal(err)
	}
	assertTotals(t, a, 1, 3)
	if _, err := b.db.Exec("UPDATE bodies SET body='bad'"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.Get("b", time.Unix(2, 0)); ok {
		t.Fatal("corrupt body served")
	}
}
func TestUnchangedPutDoesNotWriteAndPointUpdateDoesNotRewritePeers(t *testing.T) {
	s := openTest(t, t.TempDir(), 512, 1<<20)
	for i := range 512 {
		if err := s.Put(entry(fmt.Sprint(i), fmt.Sprint(i), 1, "")); err != nil {
			t.Fatal(err)
		}
	}
	changes := func() int {
		var n int
		if err := s.db.QueryRow("SELECT total_changes()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()
	if err := s.Put(entry("0", "0", 1, "")); err != nil {
		t.Fatal(err)
	}
	if got := changes() - before; got != 0 {
		t.Fatalf("unchanged save modified %d rows", got)
	}
	before = changes()
	if err := s.Put(entry("0", "changed", 2, "")); err != nil {
		t.Fatal(err)
	}
	if got := changes() - before; got > 10 {
		t.Fatalf("one update modified %d rows", got)
	}
}
func BenchmarkPointUpdate(b *testing.B) {
	for _, n := range []int{1, 512} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s, err := Open(b.TempDir(), 512, 16<<20)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			for i := range n {
				if err := s.Put(entry(fmt.Sprint(i), fmt.Sprint(i), 1, "")); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if err := s.Put(entry("0", fmt.Sprint(i), int64(i+2), "")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestDatabaseKeepsProviderResponsesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not Windows ACLs")
	}
	dir := t.TempDir()
	s := openTest(t, dir, 10, 1024)
	if err := s.Put(entry("private", "private response", 1, "")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions %o expose provider responses", info.Mode().Perm())
	}
}

func TestOpenVersionsSchemaAndRefusesNewerCache(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 8, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("SELECT version FROM schema_meta").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if _, err := s.db.Exec("UPDATE schema_meta SET version=99"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if newer, err := Open(dir, 8, 4096); err == nil {
		_ = newer.Close()
		t.Fatal("future schema accepted")
	}
}
