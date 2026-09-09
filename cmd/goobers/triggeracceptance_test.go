package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func acceptedService(t *testing.T, path string, dispatch *daemonTriggerService) *durableTriggerService {
	t.Helper()
	s, err := newDurableTriggerService(path, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.queue.Close() })
	return s
}

type acceptedRunIDStarter struct{ ids chan string }

func (s *acceptedRunIDStarter) Start(_ context.Context, request localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.ids <- request.RunID
	return localscheduler.StartResult{Phase: "completed"}, nil
}

func TestDurableTriggerPinsIdentityThroughActualScheduler(t *testing.T) {
	for _, mode := range []string{"unqualified", "exact", "priority"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.db")
			dispatch := newDaemonTriggerService()
			s := acceptedService(t, path, dispatch)
			request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
			if mode != "unqualified" {
				request.Gaggle = "own"
			}
			if mode == "priority" {
				request.SourceRun = "source"
			}
			accepted, err := s.Trigger(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, path, dispatch)
			starter := &acceptedRunIDStarter{ids: make(chan string, 1)}
			scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl", Starter: starter}}, nil)
			dispatch.AttachScheduler(scheduler)
			dispatch.AttachDispatchContext(t.Context())
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			status, err := s.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: request.Actor})
			if err != nil || status.State != "dispatching" || status.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
				t.Fatalf("status = %+v, %v", status, err)
			}
			select {
			case id := <-starter.ids:
				if id != status.RunID {
					t.Fatalf("starter ID=%q status ID=%q", id, status.RunID)
				}
			default:
				t.Fatal("scheduler did not start accepted run")
			}
		})
	}
}

func TestDurableTriggerAcceptsDuringStartupAndOutlivesRequest(t *testing.T) {
	for _, startup := range []time.Duration{35 * time.Second, 60 * time.Second} {
		t.Run(startup.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.db")
			dispatch := newDaemonTriggerService()
			now := time.Now().UTC()
			dispatch.now = func() time.Time { return now }
			s := acceptedService(t, path, dispatch)
			request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
			ctx, cancel := context.WithCancel(t.Context())
			response, err := s.Trigger(ctx, request)
			cancel()
			if err != nil || response.AcceptanceID == "" || response.State != "accepted" {
				t.Fatalf("acceptance = %+v, %v", response, err)
			}
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(startup)
			// Reopen before attaching any scheduler, proving acceptance is not
			// merely a process-local promise made during startup.
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, path, dispatch)
			recorder := &dispatchContextRecorder{}
			dispatch.dispatch = recorder
			dispatch.AttachDispatchContext(t.Context())
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			retry, err := s.Trigger(t.Context(), request)
			if err != nil || !retry.Duplicate || retry.AcceptanceID != response.AcceptanceID || retry.RunID != "run-1" || retry.State != "dispatching" {
				t.Fatalf("retry = %+v, %v", retry, err)
			}
			requestCtx, runCtx, calls := recorder.snapshot()
			if calls != 1 || requestCtx.Err() != nil || runCtx.Err() != nil {
				t.Fatalf("dispatch calls=%d, contexts=%v/%v", calls, requestCtx.Err(), runCtx.Err())
			}
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, _, calls = recorder.snapshot()
			if calls != 1 {
				t.Fatalf("repeated sweep minted %d runs", calls)
			}
		})
	}
}

func TestDurableTriggerPreservesPodAuthorityAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := newDaemonTriggerService().withGaggleContainment(func(gaggle, run string) bool { return gaggle == "own" && run == "pod-run" })
	s := acceptedService(t, path, dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "pod:pod-run", PodScoped: true, PodRunID: "pod-run"}
	response, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Close(); err != nil {
		t.Fatal(err)
	}
	// Authority changed while down. A lost json:"-" field would bypass this
	// check and accidentally dispatch the queued pod request as an operator.
	dispatch = newDaemonTriggerService().withGaggleContainment(func(string, string) bool { return false })
	recorder := &dispatchContextRecorder{}
	dispatch.dispatch = recorder
	s = acceptedService(t, path, dispatch)
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, calls := recorder.snapshot()
	if calls != 0 {
		t.Fatal("lost pod containment dispatched a run")
	}
	r, err := s.queue.Get(t.Context(), response.AcceptanceID, request.Actor)
	if err != nil || r.State != triggerqueue.Rejected {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestDurableTriggerReturnsAcceptanceWhileDispatchIsBlocked(t *testing.T) {
	dispatch := newDaemonTriggerService()
	barrier := &barrierTriggerer{entered: make(chan struct{}), release: make(chan struct{})}
	dispatch.dispatch = barrier
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
	original, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Drain(t.Context()) }()
	<-barrier.entered
	retry, err := s.Trigger(t.Context(), request)
	close(barrier.release)
	if drainErr := <-done; drainErr != nil {
		t.Fatal(drainErr)
	}
	if err != nil || !retry.Duplicate || retry.AcceptanceID != original.AcceptanceID || retry.State != "dispatching" {
		t.Fatalf("blocked retry = %+v, %v", retry, err)
	}
	if barrier.mints.Load() != 1 {
		t.Fatal("retry duplicated blocked dispatch")
	}
}

func TestDurableTriggerStatusBindsActorAndPodIdentity(t *testing.T) {
	dispatch := newDaemonTriggerService().withGaggleContainment(func(string, string) bool { return true })
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "pod:run-1", PodScoped: true, PodRunID: "run-1"}
	accepted, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	lookup := httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: request.Actor, PodScoped: true, PodRunID: "run-1"}
	status, err := s.TriggerStatus(t.Context(), lookup)
	if err != nil || status.State != "accepted" || status.AcceptanceID != accepted.AcceptanceID || status.AcceptedAt.IsZero() {
		t.Fatalf("status = %+v, %v", status, err)
	}
	for _, altered := range []httpapi.TriggerStatusRequest{
		{AcceptanceID: lookup.AcceptanceID, Actor: "other", PodScoped: true, PodRunID: "run-1"},
		{AcceptanceID: lookup.AcceptanceID, Actor: lookup.Actor, PodScoped: true, PodRunID: "run-2"},
		{AcceptanceID: lookup.AcceptanceID, Actor: lookup.Actor},
		{AcceptanceID: "missing", Actor: lookup.Actor, PodScoped: true, PodRunID: "run-1"},
	} {
		status, err := s.TriggerStatus(t.Context(), altered)
		if err == nil || status.AcceptanceID != "" || err.Error() != "trigger acceptance not found" {
			t.Fatalf("foreign status = %+v, %v", status, err)
		}
	}
}
