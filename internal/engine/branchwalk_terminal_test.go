package engine

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticBranchGateCannotCompleteRun(t *testing.T) {
	in := branchBodyInput("branch-terminal-gate", true, false)
	// This compiles: the alternate fail edge reaches @join, but the selected
	// pass edge falls off the branch into the whole-run terminal.
	in.Run.Spec.Gates[0].Branches["pass"] = wf.TerminalComplete
	if _, err := wf.Compile(wf.Definition{Name: in.Run.WorkflowName, Version: in.Run.Version, DSLVersion: in.Run.DSLVersion, Spec: in.Run.Spec}, wf.WithPreviewFeatures(true)); err != nil {
		t.Fatal(err)
	}
	runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, wantLocalErr: true, script: map[string][]scriptedCall{
		"seed":  {succeed(map[string]interface{}{"value": "frozen-root"})},
		"alpha": {succeed(map[string]interface{}{"value": "alpha"})},
		"beta":  {succeed(map[string]interface{}{"value": "beta"})},
	}}, "branch-terminal-gate-local")
	writer, _ := newLiveWriter(t)
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: &fixtureAuto{localOK: true}, Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "body"}}, nil
	}}}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(staticBranchBodies, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out branchWalkTestResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Branches[0].Status != journal.BranchFailed {
		t.Fatalf("whole-run terminal accepted by engine branch: status=%s result=%+v; local runner rejects same graph", out.Branches[0].Status, out.Branches[0].Result)
	}
	if !strings.Contains(out.Branches[0].Error, "completed the run instead of routing") || out.Branches[0].Result.Status == StatusCompleted {
		t.Fatalf("branch completion did not fail closed: %+v", out.Branches[0])
	}
}
