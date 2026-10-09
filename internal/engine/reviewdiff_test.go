package engine

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gate"
	wf "github.com/goobers/goobers/internal/workflow"
)

// readonlyImplementationReviewSpec is the #5414 shape: an agentic implementer,
// a deterministic stage between it and the review, and a reviewer that
// declared repo-readonly (detached at base).
func readonlyImplementationReviewSpec() apiv1.WorkflowSpec {
	spec := laneSpec()
	spec.Tasks[0].Next = "check"
	spec.Tasks = append(spec.Tasks, apiv1.Task{
		Name: "check", Type: apiv1.TaskDeterministic,
		Run: &apiv1.DeterministicRun{Command: []string{"make", "verify"}}, Next: "review",
	})
	spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceRepoReadOnly
	return spec
}

// TestReadonlyImplementationReviewIsHandedTheRunDiff is #5414 on the engine
// path: the reviewer's own read-only workspace cannot see the run branch, so
// the run's diff is probed from a short-lived run-branch workspace and handed
// to the reviewer as the usual "<gate>.diff" pointer.
func TestReadonlyImplementationReviewIsHandedTheRunDiff(t *testing.T) {
	var pointers []apiv1.ContextPointer
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			pointers = env.ContextPointers
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	// The probe reads first; anything the reviewer's own detached workspace
	// would report after it is empty, so a pointer proves the probe's diff.
	ws.scriptDiffSequence("review", [][]byte{[]byte("+implementation\n"), nil})
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", readonlyImplementationReviewSpec()))

	if res := laneResult(t, env); res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	found := false
	for _, p := range pointers {
		found = found || p.Name == "review.diff"
	}
	if !found {
		t.Fatalf("reviewer was not handed the run diff; pointers = %+v", pointers)
	}
	var modes []apiv1.WorkspaceMode
	for _, req := range ws.provisioned() {
		if req.Stage == "review" {
			modes = append(modes, req.Mode)
		}
	}
	if len(modes) != 2 || modes[0] != apiv1.WorkspaceRepo || modes[1] != apiv1.WorkspaceRepoReadOnly {
		t.Fatalf("review workspaces = %v, want the run-branch probe then the reviewer's own read-only workspace", modes)
	}
	// Both share the RunID+Stage key, so the probe must be gone before the
	// reviewer's workspace is provisioned.
	got := ws.lifecycleOf("review")
	want := []string{"provision review", "remove review", "provision review", "remove review"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review workspace lifecycle = %v, want %v (probe removed before the reviewer's workspace is provisioned)", got, want)
	}
}

// TestReadonlyImplementationReviewEmptyDiffFailsClosed is #5414's (c): an
// empty run diff fast-fails the implementation review even though its direct
// subject is a deterministic stage, without asking the reviewer.
func TestReadonlyImplementationReviewEmptyDiffFailsClosed(t *testing.T) {
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	ws := testWorkspaces(t)
	ws.scriptDiff("review", nil)
	env := laneEnv(t, inv, ws)
	env.ExecuteWorkflow(Run, runInput("gated", readonlyImplementationReviewSpec()))

	if reviews != 0 {
		t.Fatalf("reviewer invoked %d time(s) on an empty implementation diff", reviews)
	}
	if res := laneResult(t, env); res.Status != StatusEscalated {
		t.Fatalf("status = %q, want escalated", res.Status)
	}
}

// readonlyResearchReviewSpec is a repo-readonly agentic research task reviewed
// by a non-writable reviewer: the subject is agentic but implements nothing,
// so there is no run diff to retrieve or require.
func readonlyResearchReviewSpec(reviewer apiv1.WorkspaceMode) apiv1.WorkflowSpec {
	spec := laneSpec()
	spec.Tasks[0].Workspace = apiv1.WorkspaceRepoReadOnly
	spec.Gates[0].Agentic.Workspace = reviewer
	return spec
}

// TestReadonlyResearchReviewIsNotProbedOrFailedClosed: a review of a
// non-implementing agentic subject must run the reviewer on its own workspace,
// without a writable run-branch probe and without an empty-diff fast-fail.
func TestReadonlyResearchReviewIsNotProbedOrFailedClosed(t *testing.T) {
	for _, reviewer := range []apiv1.WorkspaceMode{apiv1.WorkspaceRepoReadOnly, apiv1.WorkspaceScratch} {
		t.Run(string(reviewer), func(t *testing.T) {
			reviews := 0
			inv := &fakeInvoker{
				invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
				},
				review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
					reviews++
					return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
				},
			}
			ws := testWorkspaces(t)
			ws.scriptDiff("review", nil)
			env := laneEnv(t, inv, ws)
			env.ExecuteWorkflow(Run, runInput("gated", readonlyResearchReviewSpec(reviewer)))

			if res := laneResult(t, env); res.Status != StatusCompleted {
				t.Fatalf("status = %q, want completed", res.Status)
			}
			if reviews != 1 {
				t.Fatalf("reviewer invoked %d time(s), want 1", reviews)
			}
			var modes []apiv1.WorkspaceMode
			for _, req := range ws.provisioned() {
				if req.Stage == "review" {
					modes = append(modes, req.Mode)
				}
			}
			if len(modes) != 1 || modes[0] != reviewer {
				t.Fatalf("review workspaces = %v, want only the reviewer's own %s workspace (no writable probe)", modes, reviewer)
			}
		})
	}
}

// TestPlacedGateRequiresDiffOnlyForImplementationReview is the pod side of the
// same rule: the attempt's ReviewRequiresDiff (which makes the pod fail closed
// with reviewer_diff_missing) is set for a scratch implementation review and
// not for a scratch review of a read-only agentic research task.
func TestPlacedGateRequiresDiffOnlyForImplementationReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject apiv1.WorkspaceMode
		want    bool
	}{
		{name: "writable implementer", subject: "", want: true},
		{name: "read-only research", subject: apiv1.WorkspaceRepoReadOnly, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := placedGateSpec()
			spec.Tasks[0].Workspace = tc.subject
			spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
			spec.Gates[0].Branches["pass"] = wf.TerminalComplete
			spec.Tasks = spec.Tasks[:1]
			in := projectionInput("placed-gate-requires-diff-"+string(tc.subject), spec)
			in.DSLVersion = "3.0"
			in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}
			in.Placements = []PinnedPlacement{remoteGatePin()}
			surrenders := surrenderStore(t)
			putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass}))
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

			executeForProjection(t, in, &Activities{
				Goober: refusingReviewer(t), Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
			}, false)

			attempts, _ := fake.recorded()
			if len(attempts) != 1 || attempts[0].Stage != "review" {
				t.Fatalf("attempts = %+v, want exactly the gate's review attempt", attempts)
			}
			if got := attempts[0].ReviewRequiresDiff; got != tc.want {
				t.Fatalf("ReviewRequiresDiff = %t, want %t", got, tc.want)
			}
		})
	}
}

// The pod surrenders #415's mechanical empty-diff verdict for a writable
// implementation reviewer that saw nothing (#5414); the engine's surrender
// validation must accept it in both disposition vocabularies.
func TestSurrenderedEmptyDiffVerdictValidates(t *testing.T) {
	for _, structured := range []bool{false, true} {
		v := gate.MechanicalVerdict(gate.EmptyDiffVerdict(), apiv1.VerdictReasonEmptyDiff, structured)
		if err := validateSurrenderedVerdict(v); err != nil {
			t.Fatalf("structured=%t: %v", structured, err)
		}
	}
}
