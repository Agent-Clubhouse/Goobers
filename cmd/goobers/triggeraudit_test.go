package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerJournalsAttributionBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	dispatch := newDaemonTriggerService()
	barrier := &barrierTriggerer{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer release()
	dispatch.dispatch = barrier
	s := acceptedService(t, filepath.Join(root, "accepted.db"), dispatch)
	s.auditLog = log
	accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "operator", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Drain(t.Context()) }()
	select {
	case <-barrier.entered:
	case err := <-done:
		t.Fatalf("dispatch returned before reaching scheduler: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not reach scheduler")
	}
	// Dispatch is blocked inside the scheduler seam: attribution must already
	// be durably readable, not merely emitted after the work finishes.
	events, err := journal.ReadInstanceLog(log.Dir())
	release()
	if drainErr := <-done; drainErr != nil {
		t.Fatal(drainErr)
	}
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v error=%v", events, err)
	}
	event := events[0]
	if event.Actor != "operator" || event.Workflow != "impl" || event.Gaggle != "own" || event.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
		t.Fatalf("attribution=%+v", event)
	}
	if event.Runner["acceptanceId"] != accepted.AcceptanceID || event.Runner["requestId"] != "delivery" || event.Runner["force"] != true {
		t.Fatalf("dispatch metadata=%+v", event.Runner)
	}
}

func TestDurableTriggerAuditFailurePreservesPendingAcceptance(t *testing.T) {
	root := t.TempDir()
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	dispatch := newDaemonTriggerService()
	recorder := &dispatchContextRecorder{}
	dispatch.dispatch = recorder
	s := acceptedService(t, filepath.Join(root, "accepted.db"), dispatch)
	s.auditLog = log
	accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(t.Context()); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("audit failure=%v", err)
	}
	_, _, calls := recorder.snapshot()
	if calls != 0 {
		t.Fatal("unaudited trigger reached scheduler")
	}
	record, err := s.queue.Get(t.Context(), accepted.AcceptanceID, "operator")
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatalf("accepted request lost: %+v, %v", record, err)
	}
	// Restoring journal availability releases the same durable acceptance.
	reopened, _, err := journal.OpenInstanceLog(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	s.auditLog = reopened
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, calls = recorder.snapshot()
	if calls != 1 {
		t.Fatalf("dispatch calls after repair=%d", calls)
	}
}
