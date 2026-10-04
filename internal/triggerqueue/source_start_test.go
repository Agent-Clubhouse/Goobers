package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestSourceBatchCursorAndStartsCommitTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := openTestStore(t, path)
	before := time.Now().UTC()
	after := before.Add(time.Minute)
	if got, err := store.SourceCursor(t.Context(), "scope", before); err != nil || !got.Equal(before) {
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
	if got, err := store.SourceCursor(t.Context(), "scope", after.Add(time.Hour)); err != nil || !got.Equal(after) {
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
	if _, err := store.SourceCursor(t.Context(), "scope", before); err != nil {
		t.Fatal(err)
	}
	_, err := store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<9998) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'old-'||x,'old-'||x,'scheduler',x,'accepted',1 FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture isolates the record-count quota from the independent byte reserve.
	if _, err := store.db.Exec(`UPDATE start_controls SET reserved_bytes=0`); err != nil {
		t.Fatal(err)
	}
	batch := SourceBatch{Key: "new-fire", Actor: "scheduler", Fingerprint: "input", Starts: []SourceStart{{[]byte("start")}}, Advance: &SourceAdvance{Scope: "scope", Before: before, After: before.Add(time.Minute)}}
	if _, _, err = store.AcceptSource(t.Context(), batch, before); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	if got, err := store.SourceCursor(t.Context(), "scope", before.Add(time.Hour)); err != nil || !got.Equal(before) {
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

func TestSourceReceiptRetainsUnfinishedStartsBeyondReplayWindow(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now().UTC()
	first, _, err := store.AcceptSource(t.Context(), SourceBatch{Key: "first", Actor: "signal", Fingerprint: "input", Starts: []SourceStart{{[]byte("start")}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(ReplayRetention + time.Hour)
	if _, _, err = store.AcceptSource(t.Context(), SourceBatch{Key: "later", Actor: "signal", Fingerprint: "empty"}, later); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SourceReceipt(t.Context(), first.Key); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginDispatch(t.Context(), first.AcceptanceIDs[0]); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(t.Context(), first.AcceptanceIDs[0], Rejected, "", "removed", later); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptSource(t.Context(), SourceBatch{Key: "maintenance", Actor: "signal", Fingerprint: "empty"}, later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SourceReceipt(t.Context(), first.Key); !errors.Is(err, sql.ErrNoRows) {
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

func TestSourceMigrationPreservesAcceptedOrdinaryRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var prior []string
	for _, migration := range migrations {
		if migration == sourceStartSchema {
			break
		}
		prior = append(prior, migration)
	}
	if len(prior) == len(migrations) {
		t.Fatal("source migration absent")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", prior); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('trigger-old','old','operator','payload','accepted',1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	old, err := store.ByKey(t.Context(), "old")
	if err != nil || old.State != Accepted {
		t.Fatal(old, err)
	}
	_, _, err = store.AcceptSource(t.Context(), SourceBatch{Key: "new", Actor: "signal", Fingerprint: "input"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
}
