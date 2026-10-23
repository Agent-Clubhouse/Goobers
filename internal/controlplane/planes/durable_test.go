package planes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type durableBackendStub struct{}

func (durableBackendStub) AcceptOrdinary(context.Context, httpapi.TriggerRequest) (httpapi.TriggerResponse, bool, error) {
	return httpapi.TriggerResponse{}, false, nil
}

type reconcileBackend struct {
	durableBackendStub
	can      bool
	observed *bool
	kind     string
	calls    int
}

func (b *reconcileBackend) CanReconcileAccepted() bool { return b.can }
func (b *reconcileBackend) ReconcileAccepted(_ context.Context, _ triggerqueue.Record, kind string) (*bool, error) {
	b.calls++
	b.kind = kind
	return b.observed, nil
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

func TestReconcileObservationGuardsDuplicateDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := NewTriggerService()
	service, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	request := httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery", Actor: "operator"}
	accepted, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	record, err := service.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileObservation(t.Context(), record, true); err != nil {
		t.Fatal(err)
	}
	got, err := service.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil || got.State != triggerqueue.Dispatched || got.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
		t.Fatalf("observed reconciliation = %+v, %v", got, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	retry, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retry.Close() })
	if retry.bootUncertain[record.ID] {
		t.Fatal("completed record retained boot uncertainty")
	}
}

func TestReconcileObservationRetriesBootUncertaintyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := NewTriggerService()
	service, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	request := httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery", Actor: "operator"}
	accepted, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	retry, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retry.Close() })
	record, err := retry.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil || !retry.bootUncertain[record.ID] {
		t.Fatalf("reopened record = %+v, boot uncertainty=%v, err=%v", record, retry.bootUncertain, err)
	}
	if err := retry.ReconcileObservation(t.Context(), record, false); err != nil {
		t.Fatal(err)
	}
	if retry.bootUncertain[record.ID] {
		t.Fatal("successful retry retained boot uncertainty")
	}
	after, err := retry.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil || after.State != triggerqueue.Accepted {
		t.Fatalf("retried record = %+v, %v", after, err)
	}
	if err := retry.ReconcileObservation(t.Context(), after, false); err != nil {
		t.Fatal(err)
	}
	still, err := retry.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil || still.State != triggerqueue.Accepted {
		t.Fatalf("second reconciliation changed record = %+v, %v", still, err)
	}
}

func TestReconcileObservationClearsBootUncertaintyOnTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := NewTriggerService()
	service, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	request := httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery", Actor: "operator"}
	accepted, err := service.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := NewDurableTriggerService(path, dispatch, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retry.Close() })
	record, err := retry.queue.Get(t.Context(), accepted.AcceptanceID, request.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.queue.Finish(t.Context(), record.ID, triggerqueue.Rejected, "", "fixture", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := retry.ReconcileObservation(t.Context(), record, false); err != nil {
		t.Fatal(err)
	}
	if retry.bootUncertain[record.ID] {
		t.Fatal("transition retained boot uncertainty")
	}
}

func TestReconcileAcceptedRecordRoutesTypedAndLegacyRecords(t *testing.T) {
	backend := &reconcileBackend{can: true}
	service, err := NewDurableTriggerService(filepath.Join(t.TempDir(), "accepted.db"), NewTriggerService(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	typed, _, err := service.queue.AcceptAdmitted(t.Context(), "typed", "operator", []byte(`{"kind":"ordinary"}`), time.Now(), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileAcceptedRecord(t.Context(), typed); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 || backend.kind != "ordinary" {
		t.Fatalf("typed route calls=%d kind=%q", backend.calls, backend.kind)
	}

	legacy, _, err := service.queue.AcceptAdmitted(t.Context(), "legacy", "operator", []byte(`{"request":{"workflow":"implementation"}}`), time.Now(), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileAcceptedRecord(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Fatalf("nil legacy observer routed to backend: %d calls", backend.calls)
	}

	if err := service.queue.BeginDispatch(t.Context(), legacy.ID); err != nil {
		t.Fatal(err)
	}
	service.SetObserver(func(context.Context, triggerqueue.Record) (bool, error) { return true, nil })
	if err := service.ReconcileObserved(t.Context()); err != nil {
		t.Fatal(err)
	}
	reconciled, err := service.queue.Get(t.Context(), legacy.ID, "operator")
	if err != nil || reconciled.State != triggerqueue.Dispatched {
		t.Fatalf("legacy reconciliation = %+v, %v", reconciled, err)
	}

	service.bootUncertain[legacy.ID] = true
	if !service.BootUncertain(legacy.ID) {
		t.Fatal("boot uncertainty accessor lost entry")
	}
	service.ClearBootUncertain(legacy.ID)
	if service.BootUncertain(legacy.ID) {
		t.Fatal("boot uncertainty clear retained entry")
	}
}

func TestCancelReceiptServiceErrorBranches(t *testing.T) {
	store, err := cancelreceipt.Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewCancelReceiptService(store, func(context.Context, httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
		return httpapi.CancelRunResult{Phase: "cancelled"}, nil
	})
	first := httpapi.CancelRunRequest{RunID: "run-1", Actor: "operator", IdempotencyKey: "cancel-1"}
	if _, err := service.Cancel(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.RunID = "run-2"
	if _, err := service.Cancel(t.Context(), conflict); !isIntervention(err, http.StatusConflict, CancelRequestConflictCode) {
		t.Fatalf("conflict error = %v", err)
	}

	pending := httpapi.CancelRunRequest{RunID: "run-3", Actor: "operator", IdempotencyKey: "cancel-2"}
	payload, err := json.Marshal(struct {
		RunID   string                   `json:"runId"`
		Request httpapi.CancelRunRequest `json:"request"`
	}{pending.RunID, pending})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Begin(t.Context(), pending.IdempotencyKey, pending.Actor, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cancel(t.Context(), pending); !isIntervention(err, http.StatusConflict, CancelOutcomeUnknownCode) {
		t.Fatalf("pending error = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cancel(t.Context(), httpapi.CancelRunRequest{RunID: "run-4", Actor: "operator", IdempotencyKey: "cancel-3"}); !isIntervention(err, http.StatusServiceUnavailable, CancelReceiptUnavailableCode) {
		t.Fatalf("unavailable error = %v", err)
	}
}

func isIntervention(err error, status int, code string) bool {
	var intervention *httpapi.InterventionError
	return errors.As(err, &intervention) && intervention.Status == status && intervention.Code == code
}

// releasingBackend frees the queue's write lock once the sweep's prune pass is
// over, so the rest of the same Drain runs against an available queue.
type releasingBackend struct {
	durableBackendStub
	release func()
}

func (b releasingBackend) SweepChildren(context.Context) error {
	b.release()
	return nil
}

func TestDrainTreatsPruneBudgetExhaustionAsResumeNextSweep(t *testing.T) {
	dispatch := &triggerStub{}
	trigger := NewTriggerService()
	trigger.AttachDispatcher(dispatch)
	path := filepath.Join(t.TempDir(), "accepted.db")
	var unlock func()
	backend := releasingBackend{release: func() {
		if unlock != nil {
			unlock()
		}
	}}
	service, err := NewDurableTriggerService(path, trigger, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.pruneBudget = time.Millisecond

	accepted, err := service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "implementation", RequestID: "delivery", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}

	// An admission in flight holds the queue's single connection, as a child
	// launch does, so the prune cannot acquire it inside its budget.
	entered, held := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := service.queue.AcceptAdmitted(context.Background(), "holder", "operator", []byte(`{"kind":"holder"}`), time.Now(), func() error {
			close(entered)
			<-held
			return errors.New("holder admission refused")
		})
		done <- err
	}()
	<-entered
	var once sync.Once
	unlock = func() { once.Do(func() { close(held) }) }
	t.Cleanup(unlock)

	if err := service.Drain(t.Context()); err != nil {
		t.Fatalf("Drain returned %v, want nil when only the prune budget was exhausted", err)
	}
	if dispatch.calls != 1 {
		t.Fatalf("dispatch calls = %d, want the pending trigger dispatched", dispatch.calls)
	}
	status, err := service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
	if err != nil || status.State != string(triggerqueue.Dispatching) {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if err := <-done; err == nil {
		t.Fatal("holder admission unexpectedly succeeded")
	}
}

func TestDrainStillReportsParentCancellation(t *testing.T) {
	trigger := NewTriggerService()
	trigger.AttachDispatcher(&triggerStub{})
	service, err := NewDurableTriggerService(filepath.Join(t.TempDir(), "accepted.db"), trigger, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain on a cancelled context = %v, want context.Canceled", err)
	}
}
