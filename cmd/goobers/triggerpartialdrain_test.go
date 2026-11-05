package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type sweepProbeTriggerer struct {
	*stubTriggerer
	cancel   context.CancelFunc
	attempts []string
}

func (s *sweepProbeTriggerer) TriggerWithDispatchContextOptions(_, _ context.Context, _ string, _ time.Time, options localscheduler.ManualTriggerOptions) (string, error) {
	s.attempts = append(s.attempts, options.RunID)
	if s.cancel != nil {
		s.cancel()
		return "", context.Canceled
	}
	return "", &localscheduler.TriggerRejectedError{Reason: localscheduler.ReasonMaxParallel}
}

func TestPendingSweepInterruptedPassResumesNextRecordAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	dispatch := newDaemonTriggerService()
	probe := &sweepProbeTriggerer{stubTriggerer: &stubTriggerer{}}
	dispatch.AttachDispatcher(probe)
	now := time.Now().UTC()
	dispatch.SetClock(func() time.Time { return now })
	service := acceptedService(t, path, dispatch)
	var records []triggerqueue.Record
	for i := range 200 {
		response, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "held", RequestID: fmt.Sprint(i), Actor: "operator"})
		if err != nil {
			t.Fatal(err)
		}
		record, err := service.queue.Get(t.Context(), response.AcceptanceID, "operator")
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		now = now.Add(time.Millisecond)
	}
	// Every pass is interrupted in its first adapter call, which proves no run
	// was created. Requeue that known-unstarted attempt to keep an exact multiple
	// of the old 100-record window outstanding; it must not pin every other item.
	for i, record := range records {
		ctx, cancel := context.WithCancel(t.Context())
		probe.cancel = cancel
		err := service.Drain(ctx)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pass %d: %v", i, err)
		}
		if len(probe.attempts) != i+1 || probe.attempts[i] != strings.TrimPrefix(record.ID, "trigger-") {
			t.Fatalf("pass %d skipped uninspected custody: attempts=%v, want next %s", i, probe.attempts, record.ID)
		}
		if err := service.queue.Requeue(t.Context(), record.ID, localscheduler.ReasonMaxParallel); err != nil {
			t.Fatal(err)
		}
		if i == 99 {
			if err := service.queue.Close(); err != nil {
				t.Fatal(err)
			}
			service = acceptedService(t, path, dispatch)
		}
	}
	for _, original := range records {
		retained, err := service.queue.Get(t.Context(), original.ID, original.Actor)
		if err != nil || retained.State != triggerqueue.Accepted || !retained.AcceptedAt.Equal(original.AcceptedAt) || string(retained.Payload) != string(original.Payload) {
			t.Fatal("interrupted traversal altered accepted custody", retained, err)
		}
	}
}

func TestPendingSweepSmallHeldQueueInspectedOncePerPass(t *testing.T) {
	dispatch := newDaemonTriggerService()
	probe := &sweepProbeTriggerer{stubTriggerer: &stubTriggerer{}}
	dispatch.AttachDispatcher(probe)
	service := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	for i := range 2 {
		if _, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "held", RequestID: fmt.Sprint(i), Actor: "operator"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		if err := service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(probe.attempts) != 2*(i+1) {
			t.Fatal("same held record inspected repeatedly in one pass", probe.attempts)
		}
	}
}
