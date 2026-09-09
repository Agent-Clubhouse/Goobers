package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
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
			if err != nil || !retry.Duplicate || retry.AcceptanceID != response.AcceptanceID || retry.RunID != "run-1" || retry.State != "dispatched" {
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
