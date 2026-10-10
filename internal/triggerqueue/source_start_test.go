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

func TestSourceBatchRollbackLeavesNoPartialRecipients(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	// Fail the second insert after the first recipient has actually been written.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_second BEFORE INSERT ON triggers WHEN CAST(NEW.payload AS TEXT)='second' BEGIN SELECT RAISE(ABORT, 'injected recipient failure'); END`); err != nil {
		t.Fatal(err)
	}
	batch := SourceBatch{Key: "delivery", Actor: "webhook", Fingerprint: "input", Starts: []SourceStart{{[]byte("first")}, {[]byte("second")}}}
	if _, _, err := s.AcceptSource(t.Context(), batch, time.Now()); err == nil {
		t.Fatal("accepted partial batch")
	}
	var count int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM triggers)+(SELECT COUNT(*) FROM source_start_receipts)`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_second`); err != nil {
		t.Fatal(err)
	}
	receipt, duplicate, err := s.AcceptSource(t.Context(), batch, time.Now())
	if err != nil || duplicate || len(receipt.AcceptanceIDs) != 2 {
		t.Fatal(receipt, duplicate, err)
	}
}

func TestSourceEmptyReceiptsShareCapacityWithOrdinaryStarts(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now()
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO source_start_receipts SELECT 'empty-'||x,'webhook','input','[]',? FROM n`, MaxRecords-1, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	batch := SourceBatch{Key: "last-empty", Actor: "webhook", Fingerprint: "input"}
	accepted, _, err := s.AcceptSource(t.Context(), batch, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(t.Context(), "ordinary", "operator", []byte("input"), now); !errors.Is(err, ErrFull) {
		t.Fatalf("ordinary bypassed receipt capacity: %v", err)
	}
	if _, _, err := s.AcceptSource(t.Context(), SourceBatch{Key: "overflow", Actor: "webhook", Fingerprint: "input"}, now); !errors.Is(err, ErrFull) {
		t.Fatalf("empty delivery bypassed capacity: %v", err)
	}
	replay, duplicate, err := s.AcceptSource(t.Context(), batch, now)
	if err != nil || !duplicate || !reflect.DeepEqual(replay, accepted) {
		t.Fatal(replay, duplicate, err)
	}
}

func TestSourceConcurrentWritersRetainOneRecipientSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	stores := []*Store{openTestStore(t, path), openTestStore(t, path)}
	batch := SourceBatch{Key: "delivery", Actor: "webhook", Fingerprint: "input", Starts: []SourceStart{{[]byte("one")}, {[]byte("two")}}}
	const writers = 8
	receipts := make([]SourceReceipt, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipts[i], _, errs[i] = stores[i%2].AcceptSource(t.Context(), batch, time.Now())
		}()
	}
	wg.Wait()
	for i := range writers {
		if errs[i] != nil || !reflect.DeepEqual(receipts[i].AcceptanceIDs, receipts[0].AcceptanceIDs) {
			t.Fatal(i, receipts[i], errs[i])
		}
	}
	var count int
	if err := stores[0].db.QueryRow(`SELECT COUNT(*) FROM triggers`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
