package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type projectionCommitSink struct{ events chan journal.CommittedEvent }

func (s projectionCommitSink) Commit(event journal.CommittedEvent) {
	select {
	case s.events <- event:
	default:
		panic("test commit queue overflow")
	}
}

func TestProjectionDoesNotExportHistoricalJournalEvents(t *testing.T) {
	root := t.TempDir()
	sink := projectionCommitSink{events: make(chan journal.CommittedEvent, 10)}
	stop, err := journal.RegisterCommittedEventSink(root, "", sink)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	at := time.Now().UTC()
	id := "0123456789abcdef0123456789abcdef"
	proj := JournalProjection{
		Identity: journal.RunIdentity{RunID: id, Workflow: "synthetic", WorkflowVersion: 1, Gaggle: "test"},
		Graph:    []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"synthetic"}`),
		Ops: []JournalOp{
			{Kind: opAppend, Time: at, Event: &journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}},
			{Kind: opAppend, Time: at.Add(time.Second), Event: &journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}},
		},
		SchedulerOps: []JournalOp{{
			Kind: opAppend, Time: at,
			Event: &journal.Event{Type: journal.EventTriggerFired, RunID: id, Workflow: "synthetic", Gaggle: "test", Reason: "scheduled"},
		}},
	}
	dir, err := ProjectRun(filepath.Join(root, "runs"), proj)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := filepath.Join(root, "scheduler")
	if err := ProjectSchedulerEvents(scheduler, proj); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("historical projection exported %d records", len(sink.events))
	}
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(scheduler)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("new live writes exported %d records, want 2", len(sink.events))
	}
	runEvent, schedulerEvent := <-sink.events, <-sink.events
	if runEvent.Kind != "run" || runEvent.Seq != 3 || schedulerEvent.Kind != "scheduler" || schedulerEvent.Seq != 2 {
		t.Fatalf("wrong live events after projection: %+v %+v", runEvent, schedulerEvent)
	}
}
