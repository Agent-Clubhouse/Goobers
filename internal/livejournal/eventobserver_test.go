package livejournal

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestWithEventObserverDeliversCommittedEvents is the seam decision 005 D1's
// mid-run rate-limit observer stands on.
//
// WithObserver — journal.WithAppendObserver's shape — delivers only
// (runID, seq). Deciding anything about WHAT was appended from that requires
// re-opening and re-reading the run journal on every single append, on a path
// that holds the run's lock. So the daemon needs the event body itself, and
// it needs it for events that actually COMMITTED: an observer fired for an
// append that then failed would let the scheduler park its provider quota on
// a rate limit no journal records.
func TestWithEventObserverDeliversCommittedEvents(t *testing.T) {
	type seen struct {
		runID string
		ev    journal.Event
	}
	var observed []seen
	w, runsDir := testWriter(t, WithEventObserver(func(runID string, ev journal.Event) {
		observed = append(observed, seen{runID, ev})
	}))

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := w.Emit(context.Background(), openBatch("run-observed", started)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// The journal's own record is the oracle: the observer must have seen
	// exactly the events that landed, no more and no fewer.
	events := readEvents(t, runsDir, "run-observed")
	if len(observed) == 0 {
		t.Fatal("no events delivered to the event observer; the daemon's mid-run rate-limit sink would never fire for an engine run")
	}
	byType := map[journal.EventType]int{}
	for _, ev := range events {
		byType[ev.Type]++
	}
	for _, got := range observed {
		if got.runID != "run-observed" {
			t.Errorf("observer got run id %q, want run-observed", got.runID)
		}
		if byType[got.ev.Type] == 0 {
			t.Errorf("observer was delivered a %q event the journal does not carry", got.ev.Type)
			continue
		}
		byType[got.ev.Type]--
	}
}

// TestWithEventObserverIsNotFiredForDeduplicatedOps: a retried activity
// replays its emissions, and an op whose idempotency key was already applied
// appends nothing. Firing the observer for it would double-count — for the
// rate-limit sink that means re-parking the scheduler's quota on every
// activity retry, extending an outage the provider already ended.
func TestWithEventObserverIsNotFiredForDeduplicatedOps(t *testing.T) {
	var count int
	w, _ := testWriter(t, WithEventObserver(func(string, journal.Event) { count++ }))

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	batch := openBatch("run-replayed", started)
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	first := count

	// The identical batch again: the emit keys are the same, so every op is
	// deduplicated.
	resp, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatalf("replayed Emit: %v", err)
	}
	if resp.Applied != 0 {
		t.Fatalf("replayed batch applied %d ops, want 0; the fixture does not reproduce a replay", resp.Applied)
	}
	if count != first {
		t.Errorf("observer fired %d extra times for a fully deduplicated replay, want 0", count-first)
	}
}

// TestWithEventObserverIsOptional: the writer is constructed on instances that
// have no quota state to record into, and must not require an observer.
func TestWithEventObserverIsOptional(t *testing.T) {
	w, runsDir := testWriter(t)
	if _, err := w.Emit(context.Background(), openBatch("run-plain", time.Now().UTC())); err != nil {
		t.Fatalf("Emit without an event observer: %v", err)
	}
	if len(readEvents(t, runsDir, "run-plain")) == 0 {
		t.Error("no events written")
	}
}
