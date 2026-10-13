package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestSourceBatchCursorAndStartsCommitTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := openTestStore(t, path)
	before := time.Now().UTC()
	after := before.Add(time.Minute)
	if got, err := testSourceCursor(t, store, "scope", before); err != nil || !got.Equal(before) {
		t.Fatal(got, err)
	}
	batch := SourceBatch{Key: "nominal-fire", Actor: "scheduler", Fingerprint: "source-input", Starts: []SourceStart{{[]byte("first")}, {[]byte("second")}}, Advance: &SourceAdvance{Scope: "scope", Before: before.Add(-time.Minute), After: after}}
	if _, _, err := store.AcceptSource(t.Context(), batch, after); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	pending, err := store.Pending(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if _, err = store.SourceReceipt(t.Context(), batch.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	batch.Advance.Before = before
	receipt, duplicate, err := store.AcceptSource(t.Context(), batch, after)
	if err != nil || duplicate || len(receipt.AcceptanceIDs) != 2 {
		t.Fatal(receipt, duplicate, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	// A lost acknowledgement observes original recipients, not a new config's payload.
	batch.Starts = []SourceStart{{[]byte("changed pinned definition")}}
	replay, duplicate, err := store.AcceptSource(t.Context(), batch, after.Add(time.Hour))
	if err != nil || !duplicate || !reflect.DeepEqual(replay, receipt) {
		t.Fatal(replay, duplicate, err)
	}
	if got, err := testSourceCursor(t, store, "scope", after.Add(time.Hour)); err != nil || !got.Equal(after) {
		t.Fatal(got, err)
	}
	batch.Fingerprint = "changed-input"
	if _, _, err = store.AcceptSource(t.Context(), batch, after); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestSourceCapacityRollbackDoesNotAdvanceCursor(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	before := time.Now().UTC()
	if _, err := testSourceCursor(t, store, "scope", before); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<9998) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'old-'||x,'old-'||x,'scheduler',x,'accepted',1 FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	batch := SourceBatch{Key: "new-fire", Actor: "scheduler", Fingerprint: "input", Starts: []SourceStart{{[]byte("start")}}, Advance: &SourceAdvance{Scope: "scope", Before: before, After: before.Add(time.Minute)}}
	if _, _, err = store.AcceptSource(t.Context(), batch, before); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	if got, err := testSourceCursor(t, store, "scope", before.Add(time.Hour)); err != nil || !got.Equal(before) {
		t.Fatal(got, err)
	}
	if _, err = store.db.Exec(`DELETE FROM triggers WHERE key='old-1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptSource(t.Context(), batch, before); err != nil {
		t.Fatal(err)
	}
	// Receipts and cursors share ordinary custody capacity, rather than an extra quota.
	if _, _, err = store.Accept(t.Context(), "ordinary", "operator", []byte("payload"), before); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
}

func TestSourceCursorAdoptsLegacyOutstandingMarkerOnlyOnce(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	before := time.Now().UTC()
	after := before.Add(time.Minute)
	cursor, pending, err := store.SourceCursorWithLegacy(t.Context(), "scope", before, true)
	if err != nil || !pending || !cursor.Equal(before) {
		t.Fatal(cursor, pending, err)
	}
	_, _, err = store.AcceptSource(t.Context(), SourceBatch{Key: "legacy", Actor: "scheduler", Fingerprint: "input", Starts: []SourceStart{{[]byte("start")}}, Advance: &SourceAdvance{Scope: "scope", Before: before, After: after}}, after)
	if err != nil {
		t.Fatal(err)
	}
	// Crash after transaction but before clearing schedule-demand.json.
	cursor, pending, err = store.SourceCursorWithLegacy(t.Context(), "scope", before, true)
	if err != nil || pending || !cursor.Equal(after) {
		t.Fatal(cursor, pending, err)
	}
}

func testSourceCursor(t *testing.T, store *Store, scope string, initial time.Time) (time.Time, error) {
	t.Helper()
	cursor, _, err := store.SourceCursorWithLegacy(t.Context(), scope, initial, false)
	return cursor, err
}

func TestSourceCursorMigrationPreservesSignalCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if migrations[len(migrations)-1] != sourceCursorSchema {
		t.Fatal("cursor migration is not additive")
	}
	if err := sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:len(migrations)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('trigger-old','old','webhook','original','accepted',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO source_start_receipts(source_key,actor,fingerprint,acceptance_ids,accepted_ns) VALUES('delivery','webhook','digest','["trigger-old"]',1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	receipt, err := store.SourceReceipt(t.Context(), "delivery")
	if err != nil || len(receipt.AcceptanceIDs) != 1 || receipt.AcceptanceIDs[0] != "trigger-old" {
		t.Fatal(receipt, err)
	}
	record, err := store.Get(t.Context(), "trigger-old", "webhook")
	if err != nil || record.State != Accepted || string(record.Payload) != "original" {
		t.Fatal(record, err)
	}
	if _, _, err := store.SourceCursorWithLegacy(t.Context(), "schedule", time.Now(), false); err != nil {
		t.Fatal(err)
	}
}

func TestSourceCursorConcurrentFiringsCommitOnlyOneAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	stores := []*Store{openTestStore(t, path), openTestStore(t, path)}
	before := time.Now().UTC()
	if _, _, err := stores[0].SourceCursorWithLegacy(t.Context(), "scope", before, false); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := SourceBatch{Key: []string{"first", "second"}[i], Actor: "scheduler", Fingerprint: "input", Starts: []SourceStart{{[]byte("original")}}, Advance: &SourceAdvance{Scope: "scope", Before: before, After: before.Add(time.Duration(i+1) * time.Minute)}}
			_, _, errs[i] = stores[i].AcceptSource(t.Context(), batch, before)
		}()
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrTransition) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	var receipts, starts int
	if err := stores[0].db.QueryRow(`SELECT (SELECT COUNT(*) FROM source_start_receipts),(SELECT COUNT(*) FROM triggers)`).Scan(&receipts, &starts); err != nil || receipts != 1 || starts != 1 {
		t.Fatal(receipts, starts, err)
	}
}
