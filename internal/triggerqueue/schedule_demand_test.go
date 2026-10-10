package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func captureDemandTest(t *testing.T, s *Store) (ScheduleDemand, time.Time) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := testSourceCursor(t, s, "scope", base); err != nil {
		t.Fatal(err)
	}
	if err := s.CaptureScheduleDemand(t.Context(), "fire", SourceAdvance{Scope: "scope", Before: base, After: base.Add(time.Hour)}, []byte("original pinned target"), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, err := s.ScheduleDemand(t.Context(), "scope")
	if err != nil {
		t.Fatal(err)
	}
	return d, base
}
func demandTransferBatch(key string, before, after int) SourceBatch {
	b := SourceBatch{Key: key, Actor: "scheduler", Fingerprint: key, Demand: &DemandTransfer{ID: "fire", Before: before, After: after}}
	for i := before; i < after; i++ {
		b.Starts = append(b.Starts, SourceStart{Payload: []byte("pinned start")})
	}
	return b
}
func TestScheduleDemandCoalescesAndTransfersExactlyOnce(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	d, base := captureDemandTest(t, s)
	if err := s.ObserveScheduleDemand(t.Context(), d.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.CaptureScheduleDemand(t.Context(), "later-fire", SourceAdvance{Scope: "scope", Before: base.Add(time.Hour), After: base.Add(2 * time.Hour)}, []byte("new generation"), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	same, err := s.ScheduleDemand(t.Context(), "scope")
	if err != nil || same.ID != d.ID || same.Count != 3 || string(same.Payload) != string(d.Payload) {
		t.Fatal(same, err)
	}
	batch := demandTransferBatch("first", 0, 2)
	first, _, err := s.AcceptSource(t.Context(), batch, base)
	if err != nil || len(first.AcceptanceIDs) != 2 {
		t.Fatal(first, err)
	}
	if _, _, err = s.AcceptSource(t.Context(), demandTransferBatch("stale", 0, 1), base); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	pending, err := s.Pending(t.Context(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	if _, _, err = s.AcceptSource(t.Context(), demandTransferBatch("last", 2, 3), base); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ScheduleDemand(t.Context(), "scope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	duplicate, replayed, err := s.AcceptSource(t.Context(), batch, base)
	if err != nil || !replayed || len(duplicate.AcceptanceIDs) != 2 || duplicate.AcceptanceIDs[0] != first.AcceptanceIDs[0] {
		t.Fatal(duplicate, replayed, err)
	}
}
func TestScheduleDemandIndependentWritersCannotTransferSameOrdinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	one := openTestStore(t, path)
	_, now := captureDemandTest(t, one)
	two := openTestStore(t, path)
	if err := one.ObserveScheduleDemand(t.Context(), "fire", 2); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, store := range []*Store{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := []string{"one", "two"}[i]
			_, _, err := store.AcceptSource(t.Context(), demandTransferBatch(key, 0, 1), now)
			results <- err
		}()
	}
	wg.Wait()
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrTransition) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
}
func TestScheduleDemandInventoryAndNoWorkCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	d, _ := captureDemandTest(t, s)
	page, err := s.ScheduleDemandPage(t.Context(), "", 1)
	if err != nil || len(page) != 1 || page[0].ID != d.ID || string(page[0].Payload) != string(d.Payload) {
		t.Fatal(page, err)
	}
	if err = s.ObserveScheduleDemand(t.Context(), d.ID, 0); err != nil {
		t.Fatal(err)
	}
	page, err = s.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
	if err = s.ObserveScheduleDemand(t.Context(), d.ID, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("completed no-work resurrected", err)
	}
}
func TestScheduleDemandCapacityRefusesWithoutAdvancingCursor(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	base := time.Now().UTC()
	if _, err := testSourceCursor(t, s, "scope", base); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<9999) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'trigger-'||x,'key-'||x,'scheduler',X'7B7D','accepted',1 FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	err = s.CaptureScheduleDemand(t.Context(), "fire", SourceAdvance{Scope: "scope", Before: base, After: base.Add(time.Hour)}, []byte("pins"), base.Add(time.Hour))
	if !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	cursor, err := testSourceCursor(t, s, "scope", base)
	if err != nil || !cursor.Equal(base) {
		t.Fatal(cursor, err)
	}
}
func TestScheduleDemandMigrationPreservesSourceCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, scheduleDemandSchema)
	if index < 1 {
		t.Fatal("schedule demand migration unavailable")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO source_start_cursors(scope,cursor_ns) VALUES('old',1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	cursor, err := testSourceCursor(t, s, "old", time.Now())
	if err != nil || cursor.UnixNano() != 1 {
		t.Fatal(cursor, err)
	}
	captureDemandTest(t, s)
}
