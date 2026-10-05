package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerDrainMaintainsEventReceiptsWithoutScheduler(t *testing.T) {
	dispatch := newDaemonTriggerService()
	service := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	now := time.Now().UTC()
	dispatch.now = func() time.Time { return now }
	request := triggerqueue.EventAcceptance{
		Producer: triggerqueue.EventProducer{Gaggle: "own", Binding: "ingress", Actor: "binding:ingress"},
		Envelope: []byte(`{"specversion":"1.0","id":"event-1","source":"/test","type":"pr.changed"}`),
		Plan:     eventing.Plan{Revision: "routing-1", Routes: []eventing.Route{{Consumer: "repair", Revision: "consumer-1", Workflow: "repair", WorkflowDigest: "workflow-1", GooberDigest: "goober-1", ConfigGeneration: "generation-1"}}},
	}
	receipt, _, err := service.queue.AcceptEvent(t.Context(), request, now)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(triggerqueue.EventRoutingDeadline)
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := service.queue.Event(t.Context(), "own", "ingress", receipt.ID)
	if err != nil || got.State != triggerqueue.EventRoutingFailed {
		t.Fatalf("routing deadline: %+v %v", got, err)
	}
	now = now.Add(triggerqueue.EventRetention)
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = service.queue.Event(t.Context(), "own", "ingress", receipt.ID)
	if err != nil || got.TombstonedAt.IsZero() || len(got.Envelope) != 0 {
		t.Fatalf("tombstone: %+v %v", got, err)
	}
	now = now.Add(triggerqueue.EventTombstoneRetention)
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.queue.Event(t.Context(), "own", "ingress", receipt.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retained forever: %v", err)
	}
}
