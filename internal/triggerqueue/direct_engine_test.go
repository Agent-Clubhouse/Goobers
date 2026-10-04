package triggerqueue

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestDirectEngineAttachmentAtomicCustodyReplayAndPruning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	now := time.Now().UTC()
	r, dup, err := s.AcceptDirectEngine(t.Context(), "engine", "engine-cli", []byte(`{"kind":"direct","request":{"gaggle":"team","workflow":"work"}}`), []byte(`{"runId":"one"}`), now)
	if err != nil || dup {
		t.Fatal(r, dup, err)
	}
	other := openTestStore(t, path)
	got, dup, err := other.AcceptDirectEngine(t.Context(), r.Key, r.Actor, r.Payload, []byte(`{"runId":"one"}`), now)
	if err != nil || !dup || got.ID != r.ID {
		t.Fatal(got, dup, err)
	}
	if _, _, err = other.AcceptDirectEngine(t.Context(), r.Key, r.Actor, r.Payload, []byte(`{"runId":"two"}`), now); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptDirectEngine(t.Context(), "oversize", r.Actor, r.Payload, bytes.Repeat([]byte("x"), MaxDirectEngineInputBytes+1), now); err == nil {
		t.Fatal("unbounded input accepted")
	}
	if _, err = s.ByKey(t.Context(), "oversize"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial receipt", err)
	}
	page, err := s.Pending(t.Context(), 100)
	if err != nil || len(page) != 0 {
		t.Fatal("direct receipt entered ordinary drain", page, err)
	}
	if count, err := s.PendingWorkflowStarts(t.Context(), WorkflowPendingLimit{Gaggle: "team", Workflow: "work"}); err != nil || count != 0 {
		t.Fatal("direct bypass was charged to scheduler worker capacity", count, err)
	}
	if err = s.BeginDispatch(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "later", now.Add(ReplayRetention+time.Hour))
	if _, err = s.DirectEngineInput(t.Context(), r.ID); err != nil {
		t.Fatal("uncertain input pruned", err)
	}
	if err = s.Finish(t.Context(), r.ID, Dispatched, "one", "", now); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "prune", now.Add(ReplayRetention+2*time.Hour))
	if _, err = s.DirectEngineInput(t.Context(), r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("confirmed input did not cascade prune", err)
	}
}

func TestDirectEngineIndependentWritersAdmitOneInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	a := openTestStore(t, path)
	b := openTestStore(t, path)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := range 8 {
		store := a
		if i%2 != 0 {
			store = b
		}
		wg.Go(func() {
			record, _, err := store.AcceptDirectEngine(t.Context(), "once", "cli", []byte("{}"), []byte("exact input"), now)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- record.ID
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("duplicate receipts", first, id)
		}
		first = id
	}
	if first == "" {
		t.Fatal("no acceptance")
	}
}

func TestDirectEngineMigrationPreservesEarlierAcceptedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The immediately preceding schema remains upgradeable regardless of other
	// additive queue migrations integrated before this one.
	index := slices.Index(migrations, directEngineSchema)
	if index < 1 {
		t.Fatal("direct engine migration unavailable")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('trigger-0123456789abcdef0123456789abcdef','before','human','{}','accepted',1)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	if _, err = s.ByKey(t.Context(), "before"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptDirectEngine(t.Context(), "after", "engine", []byte("{}"), []byte("{}"), time.Now()); err != nil {
		t.Fatal(err)
	}
}
