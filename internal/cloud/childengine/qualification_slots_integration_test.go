//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Scenario 7 gives the lent slot to another real Temporal execution. Once the
// child seals its result, Complete must claim the next slot for its parent.
// A new ordinary trigger cannot overtake that claim (#8185).
func TestChildQualificationSingleSlot(t *testing.T) {
	h := newQualificationHarness(t)
	h.SetSlotLimit(1)
	loaded := h.InstallHTTPBarrier(func(r *http.Request) bool { return r.URL.Path == apicontract.EngineChildLoadPath })
	// Hold the first real completion response after it attempts reacquisition.
	// The durable sealed result and competing-trigger refusal below prove that
	// this boundary installed a capacity claim rather than merely returning.
	completion := h.InstallHTTPBarrier(func(r *http.Request) bool { return r.URL.Path == apicontract.EngineChildCompletePath })
	parent := h.StartParent()
	if err := loaded.Wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	parked, err := h.epoch.backend.ParkedOnChild(h.ctx, []string{parent.GetID()})
	if err != nil || !parked[parent.GetID()] {
		t.Fatalf("parent was not durably parked: %v: %v", parked, err)
	}

	record, _, err := h.epoch.store.Accept(h.ctx, "qualification-slot-holder", "qualification", []byte(`{"workflow":"default-implement"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// An unpolled queue holds this genuine engine execution until termination;
	// its scheduler starter observes Temporal closure before returning the slot.
	starter := startfence.New(h.temporal.Client(), h.queue+"-slot-holder", acceptance.NewStartAdmission(h.epoch.store), &h.applied)
	config := schedstart.Config{Engine: starter, Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
		input := h.input
		input.RunID = ""
		input.AcceptanceID = r.ID
		return input, nil
	}, Terminal: func(ctx context.Context, id string) (schedstart.Outcome, error) {
		err := h.temporal.Client().GetWorkflow(ctx, id, "").Get(ctx, nil)
		if err != nil && !temporal.IsTerminatedError(err) {
			return schedstart.Outcome{}, err
		}
		return schedstart.Outcome{Phase: journal.PhaseAborted}, nil
	}}
	dispatchCtx, cancelDispatch := context.WithCancel(h.ctx)
	t.Cleanup(func() { cancelDispatch(); h.epoch.scheduler.Wait() })
	entry := h.entries()[0]
	entry.Starter = config.StarterFor(record, nil)
	holderID := strings.TrimPrefix(record.ID, "trigger-")
	if _, err := h.epoch.scheduler.TriggerPreparedOrdinary(h.ctx, dispatchCtx, entry, holderID, localscheduler.PreparedTriggerOptions{}, time.Now()); err != nil {
		t.Fatalf("parked parent did not lend its only slot: %v", err)
	}
	// Joining is registered before server/lease cleanup. Cancel the dispatch on
	// assertion failure as well as terminating the exact owned execution.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.temporal.Client().TerminateWorkflow(ctx, holderID, "", "qualification cleanup")
	})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := h.epoch.store.RunPinsOf(h.ctx, holderID)
		if err == nil {
			break
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatal("slot holder did not confirm its start: ", err)
		}
	}
	description, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, holderID, "")
	if err != nil {
		t.Fatal(err)
	}
	holderExecution := description.WorkflowExecutionInfo.Execution.RunId
	loaded.Release()
	if err := completion.Wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	var sealed int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2 AND state='completed' AND result_receipt_digest<>''`, testInstance, parent.GetID()).Scan(&sealed); err != nil || sealed != 1 {
		t.Fatalf("parent waited without a sealed child result: %d: %v", sealed, err)
	}
	if err := h.temporal.Client().TerminateWorkflow(h.ctx, holderID, holderExecution, "qualification slot holder finished"); err != nil {
		t.Fatal(err)
	}
	h.epoch.scheduler.Wait()
	competitor, _, err := h.epoch.store.Accept(h.ctx, "qualification-competing-trigger", "qualification", []byte(`{"workflow":"default-implement"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	entry.Starter = config.StarterFor(competitor, nil)
	competitorID := strings.TrimPrefix(competitor.ID, "trigger-")
	assertRefused := func() {
		t.Helper()
		_, err := h.epoch.scheduler.TriggerPreparedOrdinary(h.ctx, dispatchCtx, entry, competitorID, localscheduler.PreparedTriggerOptions{}, time.Now())
		var rejected *localscheduler.TriggerRejectedError
		if !errors.As(err, &rejected) || rejected.Reason != localscheduler.ReasonMaxParallel || !rejected.Transient() {
			t.Fatalf("competing trigger bypassed parent's capacity claim: %v", err)
		}
	}
	assertRefused() // the holder has released; only the parent's claim protects it
	completion.Release()
	qualificationCompleted(t, h, parent)
	assertRefused() // Complete reacquired the slot held by the fixture's parent
	if _, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, competitorID, ""); err == nil {
		t.Fatal("refused competitor nevertheless started")
	} else {
		var missing *serviceerror.NotFound
		if !errors.As(err, &missing) {
			t.Fatal(err)
		}
	}
	h.epoch.release()
	t.Log("qualification scenario 7: parked parent lent its sole slot to a real run; after child completion its continuation claim beat a new ordinary trigger and the parent resumed")
}
