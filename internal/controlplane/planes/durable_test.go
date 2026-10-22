package planes

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type durableBackendStub struct{}

func (durableBackendStub) AcceptOrdinary(context.Context, httpapi.TriggerRequest) (httpapi.TriggerResponse, bool, error) {
	return httpapi.TriggerResponse{}, false, nil
}
func (durableBackendStub) DrainOrdinary(context.Context, triggerqueue.Record) error { return nil }
func (durableBackendStub) DrainChild(context.Context, triggerqueue.Record) error    { return nil }
func (durableBackendStub) HasChildren() bool                                        { return false }
func (durableBackendStub) SweepChildren(context.Context) error                      { return nil }
func (durableBackendStub) ReconcileChildren(context.Context) error                  { return nil }
func (durableBackendStub) CanReconcileAccepted() bool                               { return false }
func (durableBackendStub) ReconcileAccepted(context.Context, triggerqueue.Record, string) (*bool, error) {
	return nil, nil
}

func TestDurableTriggerServiceAcceptsAndDispatchesOnce(t *testing.T) {
	dispatch := &triggerStub{}
	trigger := NewTriggerService()
	trigger.AttachDispatcher(dispatch)
	service, err := NewDurableTriggerService(filepath.Join(t.TempDir(), "accepted.db"), trigger, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	request := httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery", Actor: "operator"}
	accepted, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.AcceptanceID == "" || accepted.State != string(triggerqueue.Accepted) {
		t.Fatalf("accepted = %+v", accepted)
	}
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != string(triggerqueue.Dispatching) || status.RunID == "" || dispatch.calls != 1 {
		t.Fatalf("status=%+v dispatch calls=%d", status, dispatch.calls)
	}
}

func TestCancelReceiptServiceReplaysCompletedResult(t *testing.T) {
	store, err := cancelreceipt.Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	calls := 0
	service := NewCancelReceiptService(store, func(context.Context, httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
		calls++
		return httpapi.CancelRunResult{Phase: "cancelled", Code: "cancelled"}, nil
	})
	request := httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "cancel-1"}
	first, err := service.Cancel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Cancel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, calls)
	}
}
