package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerDrainDoesNotDispatchChildAsCatalogWorkflow(t *testing.T) {
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	starter := &acceptedRunIDStarter{ids: make(chan string, 1)}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl", Starter: starter}}, nil)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(t.Context())
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: "parent"}, StageOccurrence: "stage", InvocationKey: "call"}
	// Even a hybrid payload must not reach a matching ordinary catalog entry.
	child, _, err := s.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte(`{"kind":"child-workflow","request":{"gaggle":"own","workflow":"impl"}}`), MaxChildren: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-starter.ids:
		t.Fatalf("child incorrectly ran catalog workflow as %s", id)
	default:
	}
	receipt, err := s.queue.ForRun(t.Context(), child.RunID)
	if err != nil || receipt.State != triggerqueue.Accepted {
		t.Fatalf("child custody claimed without launcher: %+v, %v", receipt, err)
	}
}
