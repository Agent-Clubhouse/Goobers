package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestDurableTriggerWaitsThroughBudgetResetAndStoreReopen(t *testing.T) {
	for _, daily := range []bool{false, true} {
		name := "hourly"
		if daily {
			name = "daily"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.db")
			dispatch := newDaemonTriggerService()
			now := time.Now().UTC().Truncate(time.Second)
			dispatch.SetClock(func() time.Time { return now })
			starter := &acceptedRunIDStarter{ids: make(chan string, 2)}
			readiness := apiv1.ReadinessConditions{MaxRunsPerHour: 1}
			wait, reason := time.Hour, localscheduler.ReasonBudget
			if daily {
				readiness.MaxRunsPerHour = 100
				readiness.MaxRunsPerDay = 1
				wait = 24 * time.Hour
				reason = localscheduler.ReasonDailyBudget
			}
			scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "impl", Gaggle: "own", Readiness: readiness, Starter: starter}}, nil, localscheduler.WithClock(func() time.Time { return now }, time.After))
			dispatch.AttachScheduler(scheduler)
			dispatch.AttachDispatchContext(t.Context())
			if _, err := scheduler.TriggerExact(t.Context(), localscheduler.WorkflowIdentity{Workflow: "impl", Gaggle: "own"}, now); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			<-starter.ids
			service := acceptedService(t, path, dispatch)
			accepted, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "held", Actor: "operator"})
			if err != nil {
				t.Fatal(err)
			}
			if err = service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, err := service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
			if err != nil || status.State != "accepted" || status.Reason != reason || status.RunID != "" {
				t.Fatal(status, err)
			}
			if err = service.queue.Close(); err != nil {
				t.Fatal(err)
			}
			service = acceptedService(t, path, dispatch)
			if err = service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case id := <-starter.ids:
				t.Fatal("wait bypassed budget", id)
			default:
			}
			now = now.Add(wait + time.Second)
			if err = service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			status, err = service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
			if err != nil || status.State != "dispatching" || status.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") || status.Reason != "" {
				t.Fatal(status, err)
			}
			select {
			case id := <-starter.ids:
				if id != status.RunID {
					t.Fatal(id, status)
				}
			default:
				t.Fatal("accepted trigger did not resume")
			}
		})
	}
}
