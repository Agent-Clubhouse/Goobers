package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func acceptTest(t *testing.T, s *Store, key string, now time.Time) Record {
	t.Helper()
	r, duplicate, err := s.Accept(t.Context(), key, "operator", []byte(`{"workflow":"impl"}`), now)
	if err != nil || duplicate {
		t.Fatalf("accept = %+v, %v, %v", r, duplicate, err)
	}
	return r
}

func TestAcceptanceSurvivesReopenAndBindsAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trigger?ledger.db")
	s := openTestStore(t, path)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	original := acceptTest(t, s, "delivery", now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	retry, duplicate, err := s.Accept(t.Context(), original.Key, original.Actor, original.Payload, now.Add(time.Hour))
	if err != nil || !duplicate || retry.ID != original.ID || !retry.AcceptedAt.Equal(now) {
		t.Fatalf("retry = %+v, %v, %v", retry, duplicate, err)
	}
	for _, conflict := range []struct {
		actor   string
		payload []byte
	}{
		{"other", original.Payload}, {original.Actor, []byte(`{"workflow":"other"}`)},
	} {
		r, _, err := s.Accept(t.Context(), original.Key, conflict.actor, conflict.payload, now)
		if !errors.Is(err, ErrConflict) || r.ID != "" {
			t.Fatalf("conflict leaked identity: %+v, %v", r, err)
		}
	}
	if _, err := s.Get(t.Context(), original.ID, "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign lookup: %v", err)
	}
}

func TestDispatchCustodySurvivesCrashBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "triggers.db")
	s := openTestStore(t, path)
	now := time.Now().UTC()
	r := acceptTest(t, s, "delivery", now)
	if err := s.BeginDispatch(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	pending, err := s.Pending(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("uncertain dispatch replayed: %+v, %v", pending, err)
	}
	got, err := s.Get(t.Context(), r.ID, r.Actor)
	if err != nil || got.State != Dispatching {
		t.Fatalf("custody = %+v, %v", got, err)
	}
	if err := s.BeginDispatch(t.Context(), r.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("second dispatch = %v", err)
	}
	if err := s.Finish(t.Context(), r.ID, Dispatched, "run-1", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), r.ID, Rejected, "", "late refusal", now.Add(time.Hour)); !errors.Is(err, ErrTransition) {
		t.Fatalf("outcome replaced: %v", err)
	}
	got, err = s.Get(t.Context(), r.ID, r.Actor)
	if err != nil || got.State != Dispatched || got.RunID != "run-1" {
		t.Fatalf("result = %+v, %v", got, err)
	}
}

func TestConcurrentStoresAcceptAndClaimExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "triggers.db")
	stores := []*Store{openTestStore(t, path), openTestStore(t, path)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ids []string
	accepted, claimed := 0, 0
	for i := range 20 {
		wg.Go(func() {
			s := stores[i%len(stores)]
			r, duplicate, err := s.Accept(t.Context(), "delivery", "operator", []byte("payload"), time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			ids = append(ids, r.ID)
			if !duplicate {
				accepted++
			}
			mu.Unlock()
			err = s.BeginDispatch(t.Context(), r.ID)
			if err == nil {
				mu.Lock()
				claimed++
				mu.Unlock()
			} else if !errors.Is(err, ErrTransition) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if accepted != 1 || claimed != 1 || len(ids) != 20 {
		t.Fatalf("accepted=%d claimed=%d responses=%d", accepted, claimed, len(ids))
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("distinct IDs: %v", ids)
		}
	}
}

func TestCapacityPrunesOnlyExpiredTerminalRecords(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "triggers.db"))
	now := time.Now().UTC()
	// Seed the full production bound in one transaction; then exercise only
	// public writer paths to prove admission cannot discard live custody.
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < ?)
		INSERT INTO triggers(id,key,actor,payload,state,accepted_ns)
		SELECT 'id-'||x,'key-'||x,'operator',CAST('payload' AS BLOB),'accepted',? FROM n`, MaxRecords, now.Add(-2*ReplayRetention).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(t.Context(), "overflow", "operator", []byte("payload"), now); !errors.Is(err, ErrFull) {
		t.Fatalf("overflow: %v", err)
	}
	if err := s.BeginDispatch(t.Context(), "id-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginDispatch(t.Context(), "id-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), "id-2", Dispatched, "run-2", "", now.Add(-ReplayRetention-time.Second)); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "overflow", now)
	if _, err := s.Get(t.Context(), "id-2", "operator"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired terminal retained: %v", err)
	}
	r, err := s.Get(t.Context(), "id-1", "operator")
	if err != nil || r.State != Dispatching {
		t.Fatalf("uncertain custody lost: %+v %v", r, err)
	}
	for i := range 5 {
		if _, _, err := s.Accept(t.Context(), fmt.Sprintf("extra-%d", i), "operator", []byte("payload"), now); !errors.Is(err, ErrFull) {
			t.Fatalf("steady state: %v", err)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM triggers").Scan(&count); err != nil || count != MaxRecords {
		t.Fatalf("count = %d, %v", count, err)
	}
}

func TestLedgerDurabilityConfiguration(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "triggers.db"))
	for pragma, want := range map[string]string{"journal_mode": "delete", "synchronous": "2", "max_page_count": "65536"} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q, want %q: %v", pragma, got, want, err)
		}
	}
}

func TestInvalidAcceptanceHasNoDurableSideEffects(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "triggers.db"))
	for _, tc := range []struct {
		key, actor string
		payload    []byte
	}{
		{"", "operator", []byte("payload")},
		{" spaced ", "operator", []byte("payload")},
		{"control\tkey", "operator", []byte("payload")},
		{strings.Repeat("k", 257), "operator", []byte("payload")},
		{"key", strings.Repeat("a", 1025), []byte("payload")},
		{"key", "operator", nil},
		{"key", "operator", make([]byte, MaxPayloadBytes+1)},
	} {
		if _, _, err := s.Accept(t.Context(), tc.key, tc.actor, tc.payload, time.Now()); err == nil {
			t.Fatal("invalid acceptance succeeded")
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM triggers").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid input persisted: %d, %v", count, err)
	}
	if _, err := s.Pending(t.Context(), 101); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}

func TestOpenRefusesNonRegularDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("directory accepted as database")
	}
}

func TestFailedWriteDoesNotAcknowledgeAcceptance(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "triggers.db"))
	if _, err := s.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	r, duplicate, err := s.Accept(t.Context(), "delivery", "operator", []byte("payload"), time.Now())
	if err == nil || duplicate || r.ID != "" {
		t.Fatalf("write failure acknowledged: %+v, %v, %v", r, duplicate, err)
	}
	if _, err := s.db.Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "delivery", time.Now())
}

func TestFailedCommitDoesNotAcknowledgeAcceptance(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "triggers.db"))
	// The deferred constraint lets INSERT succeed and fails only at COMMIT.
	// An acknowledgment before checking Commit would falsely accept this row.
	for _, statement := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE commit_parent(id TEXT PRIMARY KEY)",
		"CREATE TABLE commit_child(id TEXT REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER fail_commit AFTER INSERT ON triggers BEGIN INSERT INTO commit_child VALUES(new.id); END",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	r, duplicate, err := s.Accept(t.Context(), "delivery", "operator", []byte("payload"), time.Now())
	if err == nil || duplicate || r.ID != "" {
		t.Fatalf("failed commit acknowledged: %+v, %v, %v", r, duplicate, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM triggers").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed commit retained row: %d, %v", count, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_commit"); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "delivery", time.Now())
}
