package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func workerBatch(key string, count, limit int) SourceBatch {
	b := SourceBatch{Key: key, Actor: "scheduler", Fingerprint: "observation", PendingLimit: &WorkflowPendingLimit{Gaggle: "own", Workflow: "worker", MaxPending: limit}}
	for range count {
		b.Starts = append(b.Starts, SourceStart{Payload: []byte(`{"target":{"gaggle":"own","workflow":"worker"}}`)})
	}
	return b
}

func TestWorkerOccupancyCountsSharedBudgetOwners(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now().UTC()
	payloads := []string{
		`{"target":{"gaggle":"own","workflow":"worker"}}`,
		`{"gaggle":"own","workflow":"worker"}`,
		`{"gaggle":"own","parentWorkflow":"worker","workflow":"generated"}`,
		`{"request":{"workflow":"worker"}}`,
		`{"target":{"gaggle":"other","workflow":"worker"}}`,
		`{"target":{"gaggle":"own","workflow":"other"}}`,
	}
	var active Record
	for i, payload := range payloads {
		record, _, err := store.Accept(t.Context(), fmt.Sprint(i), "operator", []byte(payload), now)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			active = record
		}
	}
	limit := WorkflowPendingLimit{Gaggle: "own", Workflow: "worker", MaxPending: 4}
	if got, err := store.PendingWorkflowStarts(t.Context(), limit); err != nil || got != 4 {
		t.Fatal(got, err)
	}
	if err := store.BeginDispatch(t.Context(), active.ID); err != nil {
		t.Fatal(err)
	}
	// A dispatching row without a live scheduler owner still occupies the queue.
	if got, err := store.PendingWorkflowStarts(t.Context(), limit); err != nil || got != 4 {
		t.Fatal(got, err)
	}
	limit.ActiveRunIDs = []string{strings.TrimPrefix(active.ID, "trigger-")}
	if got, err := store.PendingWorkflowStarts(t.Context(), limit); err != nil || got != 3 {
		t.Fatal(got, err)
	}
	if err := store.Finish(t.Context(), active.ID, Dispatched, strings.TrimPrefix(active.ID, "trigger-"), "", now); err != nil {
		t.Fatal(err)
	}
	limit.ActiveRunIDs = nil
	if got, err := store.PendingWorkflowStarts(t.Context(), limit); err != nil || got != 3 {
		t.Fatal(got, err)
	}
}

func TestWorkerAcceptanceRechecksOccupancyAndRollsBackWholeObservation(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now().UTC()
	batch := workerBatch("poll", 2, 2)
	if got, err := store.PendingWorkflowStarts(t.Context(), *batch.PendingLimit); err != nil || got != 0 {
		t.Fatal(got, err)
	}
	// A manual request arrives after polling and before worker acceptance.
	if _, _, err := store.Accept(t.Context(), "manual", "alice", batch.Starts[0].Payload, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AcceptSource(t.Context(), batch, now); !errors.Is(err, ErrWorkerOccupancy) {
		t.Fatal(err)
	}
	pending, err := store.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if _, err = store.SourceReceipt(t.Context(), batch.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	// The next observation captures the genuinely missing slot.
	batch = workerBatch("next-poll", 1, 2)
	first, duplicate, err := store.AcceptSource(t.Context(), batch, now)
	if err != nil || duplicate {
		t.Fatal(first, duplicate, err)
	}
	batch.PendingLimit.MaxPending = 0
	replay, duplicate, err := store.AcceptSource(t.Context(), batch, now)
	if err != nil || !duplicate || replay.AcceptanceIDs[0] != first.AcceptanceIDs[0] {
		t.Fatal(replay, duplicate, err)
	}
}

func TestConcurrentWorkerObservationsCannotOwnSameMissingCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := openTestStore(t, path)
	other := openTestStore(t, path)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for i, writer := range []*Store{store, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := writer.AcceptSource(t.Context(), workerBatch(fmt.Sprint(i), 1, 1), now)
			outcomes <- err
		}()
	}
	wg.Wait()
	close(outcomes)
	accepted, refused := 0, 0
	for err := range outcomes {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrWorkerOccupancy):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatal(accepted, refused)
	}
	pending, err := store.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
}

func TestWorkerOccupancyFailsClosedForMalformedPendingCustody(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now().UTC()
	if _, _, err := store.Accept(t.Context(), "unknown", "operator", []byte("malformed"), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AcceptSource(t.Context(), workerBatch("poll", 1, 1), now); err == nil {
		t.Fatal("unprovable occupancy accepted")
	}
}
