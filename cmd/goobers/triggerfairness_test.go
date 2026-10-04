package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerDrainVisitsEligibleStartAfterHeldBatch(t *testing.T) {
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), dispatch)
	recorder := &dispatchContextRecorder{}
	dispatch.dispatch = recorder
	dispatch.AttachDispatchContext(t.Context())
	now := time.Now().UTC()
	dispatch.now = func() time.Time { return now }
	for index := range 100 {
		_, _, err := s.queue.Accept(t.Context(), fmt.Sprintf("held-%d", index), "human", []byte(`{"kind":"`+sessioning.StartKind+`"}`), now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", RequestID: "eligible", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, calls := recorder.snapshot(); calls != 0 {
		t.Fatal("first window did not preserve FIFO")
	}
	if err = s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, calls := recorder.snapshot(); calls != 1 {
		t.Fatal("held head starved later eligible start", calls)
	}
	receipt, err := s.queue.Get(t.Context(), accepted.AcceptanceID, "operator")
	if err != nil || receipt.State != triggerqueue.Dispatching {
		t.Fatal(receipt, err)
	}
	if s.pendingCursor.ID != "" {
		t.Fatal("short final window did not wrap")
	}
	if err = s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, calls := recorder.snapshot(); calls != 1 {
		t.Fatal("uncertain eligible start replayed")
	}
	held, err := s.queue.Pending(t.Context(), 100)
	if err != nil || len(held) != 100 {
		t.Fatal("held custody changed", len(held), err)
	}
}
