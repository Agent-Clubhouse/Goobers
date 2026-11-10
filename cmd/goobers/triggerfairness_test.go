package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestPendingSweepDaemonReopenReachesEligibleStartAfterHeldHundred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	dispatch := newDaemonTriggerService()
	now := time.Now().UTC()
	dispatch.SetClock(func() time.Time { return now })
	service := acceptedService(t, path, dispatch)
	var held []httpapi.TriggerResponse
	for i := range 100 {
		response, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "worker", Gaggle: "held", Actor: "operator", RequestID: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, response)
		now = now.Add(time.Millisecond)
	}
	ready, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "worker", Gaggle: "ready", Actor: "operator", RequestID: "eligible"})
	if err != nil {
		t.Fatal(err)
	}
	starter := &acceptedRunIDStarter{ids: make(chan string, 2)}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "worker", Gaggle: "held", DisabledReason: "paused", Starter: starter}, {Workflow: "worker", Gaggle: "ready", Starter: starter}}, nil)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(t.Context())
	if err = service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-starter.ids:
		t.Fatal("first window crossed its bound or bypassed pause", id)
	default:
	}
	for _, response := range held {
		record, err := service.queue.Get(t.Context(), response.AcceptanceID, "operator")
		if err != nil || record.State != triggerqueue.Accepted || !strings.HasPrefix(record.Reason, localscheduler.ReasonDisabled) {
			t.Fatal(record.ID, record.State, record.Reason, err)
		}
	}
	if err = service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	service = acceptedService(t, path, dispatch)
	if err = service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-starter.ids:
		if id != strings.TrimPrefix(ready.AcceptanceID, "trigger-") {
			t.Fatal("changed identity", id, ready)
		}
	default:
		t.Fatal("held prefix starved later gaggle after reopen")
	}
	record, err := service.queue.Get(t.Context(), ready.AcceptanceID, "operator")
	if err != nil || record.State != triggerqueue.Dispatching {
		t.Fatal(record, err)
	}
	for _, response := range held {
		record, err := service.queue.Get(t.Context(), response.AcceptanceID, "operator")
		if err != nil || record.State != triggerqueue.Accepted {
			t.Fatal("inspection lost held custody", record.ID, err)
		}
	}
}

func TestPendingSweepDaemonContinuesAfterMalformedRecord(t *testing.T) {
	dispatch := newDaemonTriggerService()
	now := time.Now().UTC()
	dispatch.SetClock(func() time.Time { return now })
	service := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	bad, _, err := service.queue.Accept(t.Context(), "malformed", "operator", []byte("{"), now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "worker", Gaggle: "ready", Actor: "operator", RequestID: "eligible"})
	if err != nil {
		t.Fatal(err)
	}
	starter := &acceptedRunIDStarter{ids: make(chan string, 2)}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "worker", Gaggle: "ready", Starter: starter}}, nil)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(t.Context())
	if err = service.Drain(t.Context()); err == nil || !strings.Contains(err.Error(), bad.ID) {
		t.Fatal("missing malformed-owner error", err)
	}
	scheduler.Wait()
	select {
	case id := <-starter.ids:
		if id != strings.TrimPrefix(ready.AcceptanceID, "trigger-") {
			t.Fatal(id)
		}
	default:
		t.Fatal("one invalid owner blocked the entire window")
	}
	retained, err := service.queue.Get(t.Context(), bad.ID, "operator")
	if err != nil || retained.State != triggerqueue.Accepted || string(retained.Payload) != "{" {
		t.Fatal(retained, err)
	}
}
