package triggerqueue

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestPendingSweepSurvivesEveryReopenAndHeldPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := openTestStore(t, path)
	base := time.Now().UTC()
	for i := range 205 {
		if _, _, err := store.Accept(t.Context(), fmt.Sprintf("item-%d", i), "actor", []byte("pins"), base); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, want := range []int{100, 100, 5} {
		page, err := store.PendingSweep(t.Context(), 100)
		if err != nil || len(page) != want {
			t.Fatal(len(page), err)
		}
		for _, record := range page {
			if seen[record.ID] || record.State != Accepted {
				t.Fatal("repeated before tail or changed custody", record.ID)
			}
			seen[record.ID] = true
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openTestStore(t, path)
	}
	page, err := store.PendingSweep(t.Context(), 100)
	if err != nil || len(page) != 100 || !seen[page[0].ID] {
		t.Fatal("did not wrap", len(page), err)
	}
	for _, record := range page {
		if record.State != Accepted || record.RunID != "" {
			t.Fatal("cursor claimed an execution", record)
		}
	}
}

func TestPendingSweepConcurrentHandlesReserveDistinctWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	first := openTestStore(t, path)
	second := openTestStore(t, path)
	base := time.Now().UTC()
	for i := range 20 {
		if _, _, err := first.Accept(t.Context(), fmt.Sprint(i), "actor", []byte("pins"), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	pages := make(chan []Record, 2)
	failures := make(chan error, 2)
	for _, store := range []*Store{first, second} {
		wg.Go(func() { page, err := store.PendingSweep(t.Context(), 10); pages <- page; failures <- err })
	}
	wg.Wait()
	close(pages)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for page := range pages {
		if len(page) != 10 {
			t.Fatal(len(page))
		}
		for _, record := range page {
			if seen[record.ID] {
				t.Fatal("concurrent window repeated", record.ID)
			}
			seen[record.ID] = true
		}
	}
}

func TestPendingSweepCursorDoesNotDependOnLiveBoundaryRecord(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	base := time.Now().UTC()
	a, _, err := store.Accept(t.Context(), "a", "actor", []byte("pins"), base)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := store.Accept(t.Context(), "b", "actor", []byte("pins"), base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.PendingSweep(t.Context(), 1)
	if err != nil || len(page) != 1 || page[0].ID != a.ID {
		t.Fatal(page, err)
	}
	if _, err = store.db.Exec(`DELETE FROM triggers WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	// A later insertion with an earlier timestamp is picked up after wrapping.
	old, _, err := store.Accept(t.Context(), "backdated", "actor", []byte("pins"), base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	page, err = store.PendingSweep(t.Context(), 1)
	if err != nil || len(page) != 1 || page[0].ID != b.ID {
		t.Fatal(page, err)
	}
	page, err = store.PendingSweep(t.Context(), 1)
	if err != nil || len(page) != 1 || page[0].ID != old.ID {
		t.Fatal(page, err)
	}
}

func TestPendingSweepMigrationAndCancelledReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, pendingSweepSchema)
	if index < 1 {
		t.Fatal(index)
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('legacy','legacy','actor',?,'accepted',123)`, []byte("pins")); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = store.PendingSweep(ctx, 100); err == nil {
		t.Fatal("cancelled reservation succeeded")
	}
	page, err := store.PendingSweep(t.Context(), 100)
	if err != nil || len(page) != 1 || page[0].ID != "legacy" || string(page[0].Payload) != "pins" {
		t.Fatal(page, err)
	}
	if err = sqliteschema.Migrate(t.Context(), store.db, "triggerqueue", migrations[:index]); err == nil {
		t.Fatal("old binary accepted newer schema")
	}
	for _, limit := range []int{0, 101} {
		if _, err = store.PendingSweep(t.Context(), limit); err == nil {
			t.Fatal("unbounded window", limit)
		}
	}
}
