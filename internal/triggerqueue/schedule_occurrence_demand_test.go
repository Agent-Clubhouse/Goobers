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

func occurrenceCapture(id string, before time.Time) ScheduleDemandCapture {
	return ScheduleDemandCapture{ID: id, Advance: SourceAdvance{Scope: "scope", Revision: "test", Before: before, After: before.Add(time.Minute)}, Payload: []byte("pins-" + id), Independent: true, AcceptedAt: before.Add(time.Minute)}
}

func TestOccurrenceDemandKeepsEveryFireAcrossReopenAndExactRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := testSourceCursor(t, s, "scope", base); err != nil {
		t.Fatal(err)
	}
	first := occurrenceCapture("one", base)
	for i, id := range []string{"one", "two", "three"} {
		if err := s.CaptureScheduleDemand(t.Context(), occurrenceCapture(id, base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ObserveScheduleDemand(t.Context(), "one", 3); err != nil {
		t.Fatal(err)
	}
	batch := demandTransferBatch("part-one", 0, 1)
	batch.Demand.ID = "one"
	if _, _, err := s.AcceptSource(t.Context(), batch, base); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	retry := first
	retry.AcceptedAt = base.Add(time.Hour)
	if err := reopened.CaptureScheduleDemand(t.Context(), retry); err != nil {
		t.Fatal(err)
	}
	cursor, err := testSourceCursor(t, reopened, "scope", base)
	if err != nil || !cursor.Equal(base.Add(3*time.Minute)) {
		t.Fatal("retry moved cursor", cursor, err)
	}
	oldest, err := reopened.ScheduleDemand(t.Context(), "scope")
	if err != nil || oldest.ID != "one" || oldest.Count != 3 || oldest.Queued != 1 || !oldest.Independent || string(oldest.Payload) != "pins-one" {
		t.Fatal(oldest, err)
	}
	for _, change := range []func(*ScheduleDemandCapture){
		func(c *ScheduleDemandCapture) { c.Payload = []byte("changed") },
		func(c *ScheduleDemandCapture) { c.Independent = false },
		func(c *ScheduleDemandCapture) { c.Advance.Scope = "other" },
		func(c *ScheduleDemandCapture) { c.Advance.Revision = "changed" },
		func(c *ScheduleDemandCapture) { c.Advance.After = c.Advance.After.Add(time.Minute) },
	} {
		conflict := first
		change(&conflict)
		if err := reopened.CaptureScheduleDemand(t.Context(), conflict); !errors.Is(err, ErrConflict) {
			t.Fatal("changed capture replayed", err)
		}
	}
	batch = demandTransferBatch("rest-one", 1, 3)
	batch.Demand.ID = "one"
	if _, _, err := reopened.AcceptSource(t.Context(), batch, base); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CaptureScheduleDemand(t.Context(), first); !errors.Is(err, ErrTransition) {
		t.Fatal("finished occurrence resurrected", err)
	}
	oldest, err = reopened.ScheduleDemand(t.Context(), "scope")
	if err != nil || oldest.ID != "two" || oldest.Count != -1 || oldest.Queued != 0 {
		t.Fatal(oldest, err)
	}
	if err := reopened.ObserveScheduleDemand(t.Context(), "two", 0); err != nil {
		t.Fatal(err)
	}
	oldest, err = reopened.ScheduleDemand(t.Context(), "scope")
	if err != nil || oldest.ID != "three" {
		t.Fatal("no-work fire consumed another occurrence", oldest, err)
	}
}

func TestOccurrenceDemandMigrationPreservesPartlyTransferredCoalescedFire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, scheduleOccurrenceDemandSchema)
	if index < 1 {
		t.Fatal("migration missing")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	if _, err = db.Exec(`INSERT INTO source_start_cursors(scope,cursor_ns,revision) VALUES('scope',?,'test')`, base.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO schedule_demands(scope,id,payload,observed_count,queued,accepted_ns) VALUES('scope','old',?,3,1,?)`, []byte("original pins"), base.Add(-time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	// The preceding queue-fairness release may already have reserved a
	// window. Upgrading demand custody must preserve that progress too.
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('held','held','actor',?,'accepted',1),('later','later','actor',?,'accepted',2)`, []byte("held pins"), []byte("later pins")); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE pending_start_sweep SET accepted_ns=1,last_id='held' WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	window, err := s.PendingSweep(t.Context(), 100)
	if err != nil || len(window) != 1 || window[0].ID != "later" || string(window[0].Payload) != "later pins" {
		t.Fatal("migration reset sweep progress or changed queued custody", window, err)
	}
	next := occurrenceCapture("separate", base)
	if err = s.CaptureScheduleDemand(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	coalesced := occurrenceCapture("later-coalesced", base.Add(time.Minute))
	coalesced.Independent = false
	if err = s.CaptureScheduleDemand(t.Context(), coalesced); err != nil {
		t.Fatal(err)
	}
	page, err := s.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || len(page) != 2 {
		t.Fatal(page, err)
	}
	old, err := s.ScheduleDemand(t.Context(), "scope")
	if err != nil || old.ID != "old" || old.Independent || old.Count != 3 || old.Queued != 1 || string(old.Payload) != "original pins" || !old.AcceptedAt.Equal(base.Add(-time.Hour)) {
		t.Fatal(old, err)
	}
	batch := demandTransferBatch("old-rest", 1, 3)
	batch.Demand.ID = "old"
	if _, _, err = s.AcceptSource(t.Context(), batch, base); err != nil {
		t.Fatal(err)
	}
	old, err = s.ScheduleDemand(t.Context(), "scope")
	if err != nil || old.ID != "separate" || !old.Independent {
		t.Fatal(old, err)
	}
	if err = sqliteschema.Migrate(t.Context(), s.db, "triggerqueue", migrations[:index]); err == nil {
		t.Fatal("older binary accepted new custody schema")
	}
}

func TestOccurrenceDemandConcurrentCaptureAdvancesOnlyOneBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	one, two := openTestStore(t, path), openTestStore(t, path)
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := testSourceCursor(t, one, "scope", base); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, s := range []*Store{one, two} {
		wg.Go(func() {
			results <- s.CaptureScheduleDemand(t.Context(), occurrenceCapture([]string{"one", "two"}[i], base))
		})
	}
	wg.Wait()
	successful, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successful++
		} else if errors.Is(err, ErrTransition) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	page, err := one.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || successful != 1 || conflicts != 1 || len(page) != 1 {
		t.Fatal(successful, conflicts, page, err)
	}
	cursor, err := testSourceCursor(t, one, "scope", base)
	if err != nil || !cursor.Equal(base.Add(time.Minute)) {
		t.Fatal(cursor, err)
	}
}

func TestOccurrenceDemandCapacityDoesNotCoalesceOrAdvance(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	_, base := captureDemandTest(t, s)
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<9998) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'trigger-'||x,'key-'||x,'scheduler',X'7B7D','accepted',1 FROM n`); err != nil {
		t.Fatal(err)
	}
	capture := occurrenceCapture("independent", base.Add(time.Hour))
	if err := s.CaptureScheduleDemand(t.Context(), capture); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	cursor, err := testSourceCursor(t, s, "scope", base)
	if err != nil || !cursor.Equal(capture.Advance.Before) {
		t.Fatal(cursor, err)
	}
	if _, err = s.ScheduleDemandByID(t.Context(), capture.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("capacity failure retained partial custody", err)
	}
}
