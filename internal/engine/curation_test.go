package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This file is a fixture-driven dry run of the shipped backlog-curation
// workflow (issue #25, config-examples/gaggles/acme-web/workflows/
// backlog-curation.yaml): it loads the real definition and exercises it
// through the compiled machine with fakes standing in for the not-yet-built
// backlog-query stage kind (#18) and Copilot CLI harness (#19). It proves the
// DSL correctly sequences claim -> curate and that the pipeline tolerates an
// empty claim (idempotency's observable shape at this layer); the actual
// dedupe/stale/tag/split *decisions* are the curator's instructions.md
// (LLM-driven), not asserted here.

const curationConfigRoot = "../../config-examples/gaggles/acme-web"

func loadCurationWorkflow(t *testing.T) apiv1.WorkflowSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(curationConfigRoot, "workflows", "backlog-curation.yaml"))
	if err != nil {
		t.Fatalf("read backlog-curation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return w.Spec
}

func fixtureBacklog() []string {
	return []string{"201", "202", "203", "204"}
}

func curationRunInput(spec apiv1.WorkflowSpec) RunInput {
	return RunInput{
		RunID:                  "run-curation",
		Gaggle:                 "acme-web",
		WorkflowName:           "backlog-curation",
		Version:                1,
		PreviewFeaturesEnabled: true,
		Spec:                   spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

// TestBacklogCurationDryRun exercises the shipped definition end to end over a
// fixture batch of claimed backlog items.
func TestBacklogCurationDryRun(t *testing.T) {
	spec := loadCurationWorkflow(t)
	items := fixtureBacklog()

	var gotQueryInputs map[string]interface{}
	det := &fakeRunner{
		// Keyed by TaskID, not applied blindly to every deterministic
		// invocation: release-claim (issue #234) is also a deterministic
		// task in this pipeline now, and a single unconditional assignment
		// here would have gotQueryInputs clobbered by its (empty) env.Inputs
		// since it runs after query-backlog.
		run: func(_ context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			// TaskID is "<runID>:<stateName>" (engine.go), not the bare
			// stage name.
			if strings.HasSuffix(env.TaskID, ":release-claim") {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "released"}, nil
			}
			gotQueryInputs = env.Inputs
			claimed := make([]interface{}, len(items))
			for i, it := range items {
				claimed[i] = it
			}
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]interface{}{"claimed-items": claimed},
				Summary: "claimed 4 trust-labeled items",
			}, nil
		},
	}

	var gotCurateGoal string
	inv := &fakeInvoker{
		invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			gotCurateGoal = env.Goal
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "curated 4 items",
			}, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det})

	env.ExecuteWorkflow(Run, curationRunInput(spec))

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}

	// query-backlog ran first and its declared stage inputs (trust label,
	// idempotency exclusion markers) reached the invocation envelope.
	if gotQueryInputs["trustLabel"] != "goobers:approved" {
		t.Errorf("query-backlog trustLabel input = %v, want goobers:approved", gotQueryInputs["trustLabel"])
	}
	if gotQueryInputs["excludeLabels"] != "goobers:ready,goobers:needs-human" {
		t.Errorf("query-backlog excludeLabels input = %v, want the two output markers", gotQueryInputs["excludeLabels"])
	}
	queryOut, ok := res.Outputs["query-backlog"]
	if !ok || queryOut.Status != apiv1.ResultSuccess {
		t.Fatalf("query-backlog output missing or not success: %+v", queryOut)
	}
	claimed, _ := queryOut.Outputs["claimed-items"].([]interface{})
	if len(claimed) != 4 {
		t.Errorf("claimed-items count = %d, want 4", len(claimed))
	}

	// curate ran second (goober = curator per the definition) and reported its
	// outcome through the scalar result summary, without structured outputs.
	if gotCurateGoal == "" {
		t.Error("curate stage did not receive a goal")
	}
	curateOut, ok := res.Outputs["curate"]
	if !ok || curateOut.Status != apiv1.ResultSuccess {
		t.Fatalf("curate output missing or not success: %+v", curateOut)
	}
	if curateOut.Summary != "curated 4 items" {
		t.Errorf("curate summary = %q, want %q", curateOut.Summary, "curated 4 items")
	}
	if len(curateOut.Outputs) != 0 {
		t.Errorf("curate outputs = %#v, want none", curateOut.Outputs)
	}
}

// TestBacklogCurationRerunIsNoOp exercises the idempotency property's
// observable shape at the engine layer: when nothing new is claimed (every
// item already carries an output marker, so query-backlog's real
// implementation would return an empty set — #18), the pipeline still
// completes cleanly rather than erroring on an empty claim.
func TestBacklogCurationRerunIsNoOp(t *testing.T) {
	spec := loadCurationWorkflow(t)

	det := &fakeRunner{
		run: func(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]interface{}{"claimed-items": []interface{}{}},
				Summary: "no eligible items (already curated)",
			}, nil
		},
	}
	inv := &fakeInvoker{
		invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "nothing to curate",
			}, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det})

	env.ExecuteWorkflow(Run, curationRunInput(spec))

	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (empty claim must not error)", res.Status)
	}
	claimed, _ := res.Outputs["query-backlog"].Outputs["claimed-items"].([]interface{})
	if len(claimed) != 0 {
		t.Errorf("claimed-items = %v, want empty", claimed)
	}
	curateOut := res.Outputs["curate"]
	if curateOut.Summary != "nothing to curate" {
		t.Errorf("curate summary = %q, want %q", curateOut.Summary, "nothing to curate")
	}
	if len(curateOut.Outputs) != 0 {
		t.Errorf("curate outputs = %#v, want none", curateOut.Outputs)
	}
}
