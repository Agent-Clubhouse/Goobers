package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func parentEvents(t *testing.T, dir, runID string, kind string) []journal.Event {
	t.Helper()
	var out []journal.Event
	for _, event := range liveEvents(t, dir, runID) {
		if event.Stage == "parent" && (kind == "" || event.Type == journal.EventStageStarted && kind == "started" || event.Runner["kind"] == kind) {
			out = append(out, event)
		}
	}
	return out
}

func TestEngineChildSequentialChildrenAndCapacity(t *testing.T) {
	f := newChildFixture()
	f.handoffs = 2
	f.parkAckLoss, f.completeAckLoss = true, true
	env, dir := childEnvironment(t, f)
	in := childParentInput()
	env.ExecuteWorkflow("Run", in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil || res.Status != StatusCompleted {
		t.Fatalf("parent result = %+v %v", res, err)
	}
	if len(f.envelopes) != 3 || len(f.bindings) != 2 || f.parentSlots != 1 || f.childSlots != 0 {
		t.Fatalf("family did not settle: envelopes=%d bindings=%d slots=%d/%d calls=%v", len(f.envelopes), len(f.bindings), f.parentSlots, f.childSlots, f.calls)
	}
	// Each settled return is appended in admission order and never replaced.
	if got := len(f.envelopes[2].ContextPointers) - len(f.envelopes[0].ContextPointers); got != 2 {
		t.Fatalf("third attempt carries %d child returns, want 2", got)
	}
	starts := parentEvents(t, dir, in.RunID, "started")
	if len(starts) != 3 {
		t.Fatalf("parent starts = %d", len(starts))
	}
	first, _ := journal.ChildWorkflowOriginForEvent(in.RunID, starts[0])
	for i, start := range starts[1:] {
		origin, err := journal.ChildWorkflowOriginForEvent(in.RunID, start)
		if err != nil || origin.StageOccurrence != first.StageOccurrence || origin.AttemptID != journal.StageAttemptID(in.RunID, 0, "parent", start.Seq) {
			t.Fatalf("continuation %d is not bound to the same occurrence: %+v %v", i+1, origin, err)
		}
	}
	if waits, continued := parentEvents(t, dir, in.RunID, journal.ChildWaitKind), parentEvents(t, dir, in.RunID, journal.ChildContinuedKind); len(waits) != 2 || len(continued) != 2 {
		t.Fatalf("waits=%d continued=%d", len(waits), len(continued))
	}
	// Parking is bound to the acknowledged wait marker, not a guessed sequence.
	// A park retried after a lost acknowledgement repeats the same receipt.
	var parked, committed []uint64
	for _, park := range f.parks {
		if !slices.Contains(parked, park.Wait.Seq) {
			parked = append(parked, park.Wait.Seq)
		}
	}
	for _, wait := range parentEvents(t, dir, in.RunID, journal.ChildWaitKind) {
		committed = append(committed, wait.Seq)
	}
	if !slices.Equal(parked, committed) {
		t.Fatalf("parks bound to %v, waits committed at %v", parked, committed)
	}
}

func TestEngineChildFailedResultReachesParent(t *testing.T) {
	f := newChildFixture()
	f.failChild = true
	env, _ := childEnvironment(t, f)
	env.ExecuteWorkflow("Run", childParentInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.terminal {
		if r.Input.State != "failed" {
			t.Fatalf("failed child sealed as %q", r.Input.State)
		}
	}
	if len(f.envelopes) != 2 {
		t.Fatalf("parent did not receive the failed child's bounded result: %v", f.calls)
	}
}

func TestEngineChildCustodyLossNeverResumesParent(t *testing.T) {
	f := newChildFixture()
	f.corrupt = true
	env, _ := childEnvironment(t, f)
	env.ExecuteWorkflow("Run", childParentInput())
	if env.GetWorkflowError() == nil || len(f.envelopes) != 1 || !slices.Contains(f.calls, "cancel") || slices.Contains(f.calls, "complete") {
		t.Fatalf("corrupt manifest resumed or skipped cleanup: err=%v calls=%v", env.GetWorkflowError(), f.calls)
	}
}

func TestEngineChildDurableWaitDeadline(t *testing.T) {
	f := newChildFixture()
	f.blockChild = true
	f.waitDuration = time.Minute
	env, _ := childEnvironment(t, f)
	env.ExecuteWorkflow("Run", childParentInput())
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "deadline expired") || len(f.envelopes) != 1 || slices.Contains(f.calls, "complete") || !slices.Contains(f.calls, "cancel") {
		t.Fatalf("expired wait resumed the parent: err=%v calls=%v", err, f.calls)
	}
	if f.parentSlots != 0 {
		t.Fatal("expired wait restored parent capacity without settlement")
	}
}

func TestEngineChildParentCancellationSettlesFamily(t *testing.T) {
	f := newChildFixture()
	f.blockChild = true
	env, _ := childEnvironment(t, f)
	env.RegisterDelayedCallback(env.CancelWorkflow, 10*time.Second)
	env.ExecuteWorkflow("Run", childParentInput())
	if env.GetWorkflowError() == nil || len(f.envelopes) != 1 || slices.Contains(f.calls, "complete") || !slices.Contains(f.calls, "cancel") {
		t.Fatalf("cancelled parent continued: err=%v calls=%v", env.GetWorkflowError(), f.calls)
	}
}

func TestEngineChildContinuationIntegrity(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("admission/remote=%v", remote), func(t *testing.T) {
			f := newChildFixture()
			env, _ := childEnvironment(t, f)
			in := childParentInput()
			routeChildParentRemote(env, f, &in, remote)
			in.Spec.Tasks[0].MinimumIntegrity = apiv1.IntegrityMaintainer
			env.ExecuteWorkflow("Run", in)
			if env.GetWorkflowError() == nil || len(f.envelopes) != 1 {
				t.Fatalf("unapproved child return reached a maintainer-only stage: %v", f.calls)
			}
		})
		t.Run(fmt.Sprintf("provenance/remote=%v", remote), func(t *testing.T) {
			f := newChildFixture()
			f.parentIntegrity = apiv1.IntegrityTrusted
			env, dir := childEnvironment(t, f)
			in := childParentInput()
			routeChildParentRemote(env, f, &in, remote)
			env.ExecuteWorkflow("Run", in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			for _, event := range parentEvents(t, dir, in.RunID, "") {
				if event.Type == journal.EventStageFinished && event.Integrity != apiv1.IntegrityUnapproved {
					t.Fatalf("parent output graded %q despite consuming an unapproved child return", event.Integrity)
				}
			}
		})
	}
}

func routeChildParentRemote(env *testsuite.TestWorkflowEnvironment, f *childFixture, in *RunInput, remote bool) {
	if !remote {
		return
	}
	in.Placements = []PinnedPlacement{{Stage: "parent", Queue: "child-integrity", Eligible: remoteEligible()}}
	env.RegisterActivityWithOptions(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		return f.Invoke(ctx, input.Envelope, input.WorkspaceBranch, input.WorkspaceDelta, input.Workspace, "")
	}, activity.RegisterOptions{Name: ActDispatchStage})
}

func TestEngineChildUsageCapStopsHandoff(t *testing.T) {
	f := newChildFixture()
	f.usage = &AttemptUsage{Reported: true, Metrics: map[string]float64{"tokens": 500}}
	env, _ := childEnvironment(t, f)
	in := childParentInput()
	in.Spec.Tasks[0].Limits = &apiv1.Limits{MaxTokens: 100}
	env.ExecuteWorkflow("Run", in)
	if env.GetWorkflowError() == nil || slices.Contains(f.calls, "park") || !slices.Contains(f.calls, "cancel") {
		t.Fatalf("exhausted parent budget still started a child: err=%v calls=%v", env.GetWorkflowError(), f.calls)
	}
}

func TestEngineChildOccurrenceBound(t *testing.T) {
	f := newChildFixture()
	f.handoffs = 2
	env, _ := childEnvironment(t, f)
	in := childParentInput()
	in.Spec.Tasks[0].ChildWorkflows.MaxChildren = 1
	env.ExecuteWorkflow("Run", in)
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "child limit") || len(f.bindings) != 1 {
		t.Fatalf("occurrence bound not enforced: err=%v bindings=%d", err, len(f.bindings))
	}
}

func TestEngineChildLegacyExecutionMarkerKeepsFence(t *testing.T) {
	f := newChildFixture()
	env, _ := childEnvironment(t, f)
	env.OnGetVersion(childExecutionChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.ExecuteWorkflow("Run", childParentInput())
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), errChildExecutionUnavailable.Error()) || !slices.Equal(f.calls, []string{"dispatch", "admit", "cancel"}) {
		t.Fatalf("unmarked history left the HA-P4-08 fence: err=%v calls=%v", err, f.calls)
	}
}

func TestEngineChildFrozenInputRefusals(t *testing.T) {
	binding, _ := encodedManifest(t, nil)
	t.Run("production Run", func(t *testing.T) {
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.RegisterWorkflow(Run)
		env.ExecuteWorkflow(Run, RunInput{RunID: binding.Child.RunID, Gaggle: "web", FrozenChild: &binding, LiveJournal: true, PreviewFeaturesEnabled: boolPointer(true)})
		if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "remains disabled") {
			t.Fatalf("production Run executed a frozen child: %v", err)
		}
	})
	t.Run("protocol without parent lineage", func(t *testing.T) {
		f := newChildFixture()
		env, _ := childEnvironment(t, f)
		env.ExecuteWorkflow("Run", RunInput{RunID: binding.Child.RunID, Gaggle: "web", FrozenChild: &binding, LiveJournal: true, PreviewFeaturesEnabled: boolPointer(true)})
		if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "lineage mismatch") || slices.Contains(f.calls, "load") {
			t.Fatalf("orphan frozen child ran: %v %v", err, f.calls)
		}
	})
	t.Run("inline spec with a reference", func(t *testing.T) {
		f := newChildFixture()
		env, _ := childEnvironment(t, f)
		in := childParentInput()
		in.FrozenChild = &binding
		env.ExecuteWorkflow("Run", in)
		if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous child input accepted: %v", err)
		}
	})
}
