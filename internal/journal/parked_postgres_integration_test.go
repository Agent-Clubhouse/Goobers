//go:build cloudintegration

package journal_test

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// The cloud counterpart of the local reconcile's ReconciledRun.Suspended: a
// run is parked exactly when journal.ParkedOnChild holds over its committed
// events, read as the controller role. A human gate holds its slot; a run
// without a journal yet cannot have parked.
func TestParkedOnChildReadsCommittedChildWaits(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	clock := &writerClock{}

	parked := "00000000000000000000000000081001"
	handle, owner := createEngineRunHandle(t, backend, parked, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, parked, now.Add(-30*time.Minute), time.Time{})

	resumed := "00000000000000000000000000081002"
	handle, owner = createEngineRunHandle(t, backend, resumed, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, resumed, now.Add(-30*time.Minute), now.Add(-10*time.Minute))

	gated := "00000000000000000000000000081003"
	handle, owner = createEngineRunHandle(t, backend, gated, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{Type: journal.EventGatePaused, Stage: "review"}}); err != nil {
		t.Fatal(err)
	}

	running := "00000000000000000000000000081004"
	createEngineRunHandle(t, backend, running, now.Add(-time.Hour), now.Add(-time.Hour), clock)

	unjournaled := "00000000000000000000000000081005"
	got, err := backend.ParkedOnChild(t.Context(), []string{parked, resumed, gated, running, unjournaled, parked})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{parked: true, resumed: false, gated: false, running: false, unjournaled: false}
	if len(got) != len(want) {
		t.Fatalf("parked = %v, want exactly %v", got, want)
	}
	for id, parked := range want {
		if value, ok := got[id]; !ok || value != parked {
			t.Fatalf("parked[%s] = %v (present %v), want %v; all %v", id, value, ok, parked, got)
		}
	}
	if empty, err := backend.ParkedOnChild(t.Context(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty lookup = %v, %v", empty, err)
	}
	if _, err := backend.ParkedOnChild(t.Context(), []string{parked, "not/a-run"}); !errors.Is(err, journal.ErrNotRunDirectory) {
		t.Fatalf("lookup with an invalid run id = %v, want refused", err)
	}
}

// The accepted over-admission bound (#8085 d). A parent's capacity is
// reacquired by the engine's Complete before the parent journals
// child.workflow.continued. A successor epoch that rebuilds its inventory in
// that window reads the parent as parked, so it does not count the parent's
// slot: at most one slot per such parent is over-admitted, and only until
// the continued event commits, which ends the window.
func TestParkedOnChildOverAdmitWindowEndsAtTheContinuedEvent(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	clock := &writerClock{}
	parent := "00000000000000000000000000081011"
	handle, owner := createEngineRunHandle(t, backend, parent, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, parent, now.Add(-30*time.Minute), time.Time{})
	// Complete has reacquired the slot; the continued event is not yet committed.
	if got, err := backend.ParkedOnChild(t.Context(), []string{parent}); err != nil || !got[parent] {
		t.Fatalf("in the window: parked = %v, %v; want parked (the documented over-admission)", got, err)
	}
	events, err := backend.ReadEvents(t.Context(), journal.RunRef{ID: parent}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	header, _, err := journal.PendingChildWait(recordsToEvents(events))
	if err != nil || header == nil {
		t.Fatalf("pending wait = %v, %v", header, err)
	}
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{
		Type: journal.EventRunnerAnnotation, Stage: "build", Attempt: 1,
		Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": header.Request.RequestID},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, err := backend.ParkedOnChild(t.Context(), []string{parent}); err != nil || got[parent] {
		t.Fatalf("after the continued event: parked = %v, %v; want counted again", got, err)
	}
}

func recordsToEvents(records []journal.EventRecord) []journal.Event {
	events := make([]journal.Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	return events
}
