package runner

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type dispositionWaitFixture struct {
	blocked   *ChildDispositionWaitError
	cancel    context.CancelFunc
	suspended int
}

func (f *dispositionWaitFixture) Await(context.Context, apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	panic("unused")
}
func (f *dispositionWaitFixture) Yield(context.Context, ChildHandoffRequest, ChildWorkspaceCustody) error {
	return f.blocked
}
func (f *dispositionWaitFixture) Wait(context.Context, ChildHandoffRequest) (ChildHandoffCompletion, error) {
	panic("unused")
}
func (f *dispositionWaitFixture) SuspendChildParent(context.Context, string) (ChildParentSuspension, error) {
	f.suspended++
	f.cancel()
	return nil, nil
}

func TestChildDispositionPreparationCanContinueButPublishedPlanStaysParked(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "unplanned", true: "published"}[published], func(t *testing.T) {
			r, run, frame, events, _ := recordedChildWait(t)
			request, _, err := ParkedChildRequest(events)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &dispositionWaitFixture{blocked: &ChildDispositionWaitError{Reason: "bounded host refusal", Reconcile: published}, cancel: cancel}
			r.cfg.ChildHandoff, r.cfg.ChildParentCapacity = f, f
			issue, err := r.yieldChildCustody(ctx, &frame, 1, "", request, ChildWorkspaceCustody{})
			if published {
				if !errors.Is(err, context.Canceled) || f.suspended != 1 || issue != "" {
					t.Fatal(issue, err, f.suspended)
				}
			} else if err != nil || issue != f.blocked.Reason || f.suspended != 0 {
				t.Fatal(issue, err, f.suspended)
			}
			reader, _ := journal.OpenReadOnly(run.Dir())
			after, err := reader.Events()
			if err != nil || !ParkedOnChild(after) {
				t.Fatal("refusal erased durable custody", err)
			}
			last := after[len(after)-1]
			if last.Runner["kind"] != "child.workflow.disposition-blocked" || last.Runner["reconciliationRequired"] != published {
				t.Fatal(last)
			}
		})
	}
}
