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

// Each run is projected from its own events alone: a run with no child
// events that sorts after a parked one is not parked.
func TestParkedOnChildProjectsEachRunFromItsOwnEvents(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	clock := &writerClock{}
	parked := "00000000000000000000000000082001"
	handle, owner := createEngineRunHandle(t, backend, parked, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, parked, now.Add(-30*time.Minute), time.Time{})
	plain := "00000000000000000000000000082002"
	handle, owner = createEngineRunHandle(t, backend, plain, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	// An event the projection reads but that leaves a wait untouched: carried
	// over from the parked run, the parked run's wait would still project.
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "operator.note"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := backend.ParkedOnChild(t.Context(), []string{plain, parked})
	if err != nil || !got[parked] || got[plain] {
		t.Fatalf("parked = %v, %v; want only %s parked", got, err, parked)
	}
}

// A stored event this binary cannot read fails the lookup, as every journal
// read does; it is never projected as if absent.
func TestParkedOnChildRefusesAnUnknownEventSchema(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	clock := &writerClock{}
	parked := "00000000000000000000000000083001"
	handle, owner := createEngineRunHandle(t, backend, parked, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, parked, now.Add(-30*time.Minute), time.Time{})
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_event SET body=$1
		WHERE instance_id=$2 AND run_id=$3 AND type='stage.started'`,
		[]byte(`{"schema":"goobers.dev/event/v999","type":"stage.started","stage":"build","attempt":1}`), config.InstanceID, parked); err != nil {
		t.Fatal(err)
	}
	if got, err := backend.ParkedOnChild(t.Context(), []string{parked}); !errors.Is(err, journal.ErrJournalMigrationRequired) || got != nil {
		t.Fatalf("lookup over an unknown event schema = %v, %v; want ErrJournalMigrationRequired", got, err)
	}
	// The lookup reads only the projection's event types (L1 runs it every
	// tick): a row of a type it never reads, here run.started, is not read.
	other := "00000000000000000000000000083002"
	handle, owner = createEngineRunHandle(t, backend, other, now.Add(-time.Hour), now.Add(-time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, other, now.Add(-30*time.Minute), time.Time{})
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_event SET body=$1
		WHERE instance_id=$2 AND run_id=$3 AND type='run.started'`,
		[]byte(`{"schema":"goobers.dev/event/v999","type":"run.started"}`), config.InstanceID, other); err != nil {
		t.Fatal(err)
	}
	if got, err := backend.ParkedOnChild(t.Context(), []string{other}); err != nil || !got[other] {
		t.Fatalf("lookup over an unread event type = %v, %v; want the projection read without it", got, err)
	}
}
