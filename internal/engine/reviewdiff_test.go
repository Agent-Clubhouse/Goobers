package engine

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
