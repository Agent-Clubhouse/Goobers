package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerReconcilesPublishedRunAfterLostDispatchAcknowledgment(t *testing.T) {
	for _, admissionRecorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost scheduler response", true: "asynchronous starter"}[admissionRecorded], func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			path := filepath.Join(layout.Root, "accepted.db")
			dispatch := newDaemonTriggerService()
			s := acceptedService(t, path, dispatch)
			request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "operator"}
			accepted, err := s.Trigger(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
			if admissionRecorded {
				if err := s.queue.RecordDispatch(t.Context(), accepted.AcceptanceID, runID); err != nil {
					t.Fatal(err)
				}
			}
			s.observe = acceptedTriggerObserver(layout)
			if err := s.reconcileObserved(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, err := s.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
			if err != nil || before.State != triggerqueue.Dispatching {
				t.Fatalf("unpublished run acknowledged: %+v, %v", before, err)
			}
			run, err := journal.Create(layout.ForGaggle("own").RunsDir(), journal.RunIdentity{
				RunID: runID, Workflow: "impl", WorkflowVersion: 1, Gaggle: "own",
				WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goober")), Trigger: journal.Trigger{Kind: journal.TriggerManual},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			// A new daemon knows only the durable queue and run journal.
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, path, dispatch)
			s.observe = acceptedTriggerObserver(layout)
			recorder := &dispatchContextRecorder{}
			dispatch.dispatch = recorder
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, _, calls := recorder.snapshot()
			if calls != 0 {
				t.Fatal("reconciliation started the published run twice")
			}
			status, err := s.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: request.Actor})
			if err != nil || status.State != "dispatched" || status.RunID != runID {
				t.Fatalf("reconciled status = %+v, %v", status, err)
			}
		})
	}
}
