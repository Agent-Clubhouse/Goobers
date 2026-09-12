package cancelreceipt

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenRecordsSchemaVersionAndRejectsNewerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("schema version = %d, want %d", version, len(migrations))
	}
	if _, err := s.db.Exec(`UPDATE schema_meta SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "newer than this build supports") || !strings.Contains(err.Error(), "upgrade this binary") {
		t.Fatalf("Open newer schema error = %v, want actionable refusal", err)
	}
}

func TestReceiptConcurrentReservationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var starts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			s := a
			if i%2 == 0 {
				s = b
			}
			_, fresh, err := s.Begin(t.Context(), "key", "operator", []byte("run-1"), now)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				starts.Add(1)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("executions=%d", starts.Load())
	}
	if err := a.Finish(t.Context(), "key", []byte(`{"code":"aborted"}`), now); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	receipt, fresh, err := c.Begin(t.Context(), "key", "operator", []byte("run-1"), now)
	if err != nil || fresh || !receipt.Complete || string(receipt.Result) != `{"code":"aborted"}` {
		t.Fatalf("receipt=%+v fresh=%v err=%v", receipt, fresh, err)
	}
	for _, input := range []struct{ actor, payload string }{{"other", "run-1"}, {"operator", "run-2"}} {
		if _, _, err := c.Begin(t.Context(), "key", input.actor, []byte(input.payload), now); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict=%v", err)
		}
	}
}

func TestUnfinishedReceiptSurvivesRestartAndAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, fresh, err := s.Begin(t.Context(), "key", "operator", []byte("run"), now); err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	later := now.Add(2 * Retention)
	if _, _, err := s.Begin(t.Context(), "new", "operator", []byte("new"), later); err != nil {
		t.Fatal(err)
	}
	receipt, fresh, err := s.Begin(t.Context(), "key", "operator", []byte("run"), later)
	if err != nil || fresh || receipt.Complete {
		t.Fatalf("receipt=%+v fresh=%v err=%v", receipt, fresh, err)
	}
}

func TestReceiptStorageFailureNeverAcknowledges(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "receipts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now()
	if _, _, err := s.Begin(t.Context(), "existing", "operator", []byte("run"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.Begin(t.Context(), "new", "operator", []byte("run"), now); err == nil || fresh {
		t.Fatalf("write failure fresh=%v err=%v", fresh, err)
	}
	if err := s.Finish(t.Context(), "existing", []byte(`{}`), now); err == nil {
		t.Fatal("acknowledged failed completion")
	}
	if _, err := s.db.Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	r, fresh, err := s.Begin(t.Context(), "existing", "operator", []byte("run"), now)
	if err != nil || fresh || r.Complete {
		t.Fatalf("receipt=%+v fresh=%v err=%v", r, fresh, err)
	}
}

func TestReceiptCapacityPreservesPendingAndPrunesOnlyFinished(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "receipts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now()
	_, err = s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
		INSERT INTO cancellations(key,actor,payload,created_ns) SELECT 'key-'||x,'operator',CAST('run' AS BLOB),? FROM n`, MaxRecords, now.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * Retention)
	if _, _, err := s.Begin(t.Context(), "new", "operator", []byte("run"), later); !errors.Is(err, ErrFull) {
		t.Fatalf("full=%v", err)
	}
	if _, fresh, err := s.Begin(t.Context(), "key-1", "operator", []byte("run"), later); err != nil || fresh {
		t.Fatalf("duplicate at capacity: fresh=%v err=%v", fresh, err)
	}
	if err := s.Finish(t.Context(), "key-2", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.Begin(t.Context(), "new", "operator", []byte("run"), later); err != nil || !fresh {
		t.Fatalf("admission after prune: fresh=%v err=%v", fresh, err)
	}
	if r, fresh, err := s.Begin(t.Context(), "key-1", "operator", []byte("run"), later); err != nil || fresh || r.Complete {
		t.Fatalf("pending evicted: %+v fresh=%v err=%v", r, fresh, err)
	}
}
