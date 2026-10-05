package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type slowPassTrigger struct {
	dispatchContextRecorder
	execution context.Context
	calls     int
}

func (s *slowPassTrigger) TriggerWithDispatchContextOptions(admission, execution context.Context, _ string, _ time.Time, _ localscheduler.ManualTriggerOptions) (string, error) {
	s.execution = execution
	s.calls++
	if s.calls == 1 {
		<-admission.Done()
		return "", admission.Err()
	}
	return "observed-later", nil
}

func TestDrainPassDeadlineRotatesPastSlowAdmissionWithoutCancellingExecution(t *testing.T) {
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	slow := &slowPassTrigger{}
	dispatch.dispatch = slow
	now := time.Now().UTC()
	dispatch.now = func() time.Time { return now }
	first, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "one", Actor: "human", RequestID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Millisecond)
	second, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "two", Actor: "human", RequestID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := time.Now()
	if err = s.drainWithin(lifetime, 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second || slow.calls != 1 || s.pendingCursor.ID != first.AcceptanceID {
		t.Fatal("sweep did not yield with progress", slow.calls, s.pendingCursor)
	}
	if slow.execution.Err() != nil {
		t.Fatal("pass timeout cancelled admitted lifetime")
	}
	r, err := s.queue.Get(t.Context(), first.AcceptanceID, "human")
	if err != nil || r.State != triggerqueue.Dispatching {
		t.Fatal("uncertainty was released", r, err)
	}
	r, err = s.queue.Get(t.Context(), second.AcceptanceID, "human")
	if err != nil || r.State != triggerqueue.Accepted {
		t.Fatal("later custody changed", r, err)
	}
	if err = s.Drain(lifetime); err != nil {
		t.Fatal(err)
	}
	if slow.calls != 2 {
		t.Fatal("later request starved or uncertain request replayed", slow.calls)
	}
	cancel()
	if !errors.Is(slow.execution.Err(), context.Canceled) {
		t.Fatal("execution detached from daemon cancellation")
	}
}

func TestRestartWaitingClassificationDoesNotPersistProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want triggerqueue.WaitingReason
	}{
		{errors.New("provider failed with secret-token"), triggerqueue.WaitingValidation},
		{interactiveaccess.ErrDenied, triggerqueue.WaitingAccess},
		{restartRefusal("run_not_admitted", "secret"), triggerqueue.WaitingCapacity},
		{restartRefusal("restart_source_changed", "secret"), triggerqueue.WaitingSource},
		{restartRefusal("restart_source_active", "secret"), triggerqueue.WaitingSourceBusy},
		{restartRefusal("restart_execution_unavailable", "secret"), triggerqueue.WaitingUnsupported},
	} {
		if got := restartWaitingReason(tc.err); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}

func TestDrainPassDeadlineIncludesConcurrentOwnerWait(t *testing.T) {
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	if err := s.drainWithin(t.Context(), 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
