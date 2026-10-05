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

func TestHumanRestartIndependentWritersAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	a, b := openTestStore(t, path), openTestStore(t, path)
	req := HumanRestartAcceptance{Key: "epoch", Actor: "human", Gaggle: "g", SourceRun: "source", Stage: "work", Epoch: "epoch", TerminalSequence: 7, Payload: []byte("{}"), Plan: []byte("plan")}
	now := time.Now()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := range 8 {
		q := a
		if i%2 != 0 {
			q = b
		}
		wg.Go(func() {
			r, _, err := q.AcceptHumanRestart(t.Context(), req, now)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.ID
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("duplicate", first, id)
		}
		first = id
	}
	req.Key = "oversize"
	req.Epoch = "oversize"
	req.Plan = bytes.Repeat([]byte("x"), (4<<20)+1)
	if _, _, err := a.AcceptHumanRestart(t.Context(), req, now); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := a.ByKey(t.Context(), req.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial acceptance", err)
	}
}

func TestHumanRestartMigrationAndUncertainRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, humanRestartSchema)
	if index < 1 {
		t.Fatal("migration missing")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	q := openTestStore(t, path)
	now := time.Now()
	req := HumanRestartAcceptance{Key: "epoch", Actor: "human", Gaggle: "g", SourceRun: "source", Stage: "work", Epoch: "epoch", TerminalSequence: 7, Payload: []byte("{}"), Plan: []byte("plan")}
	r, _, err := q.AcceptHumanRestart(t.Context(), req, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.BeginDispatch(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, q, "later", now.Add(ReplayRetention+time.Hour))
	if _, err = q.HumanRestartPlan(t.Context(), r.ID); err != nil {
		t.Fatal("uncertain plan expired", err)
	}
	if err = q.Finish(t.Context(), r.ID, Dispatched, "epoch", "", now); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, q, "prune", now.Add(ReplayRetention+2*time.Hour))
	if _, err = q.HumanRestartPlan(t.Context(), r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("settled attachment leaked", err)
	}
}
