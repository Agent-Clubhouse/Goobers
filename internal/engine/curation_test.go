package engine

import (
	"context"
	"os"
	"path/filepath"
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

// fixtureBacklogItem is a minimal stand-in for a claimed GitHub issue, covering
// the scenarios issue #25's acceptance criteria calls out: a duplicate pair, a
// stale item, and an oversized epic to split.
type fixtureBacklogItem struct {
	id          string
	title       string
	ageDays     int
	duplicateOf string
	oversized   bool
}

func fixtureBacklog() []fixtureBacklogItem {
	return []fixtureBacklogItem{
		{id: "201", title: "Add dark mode toggle", ageDays: 10},
		{id: "202", title: "Add dark mode switch", ageDays: 3, duplicateOf: "201"},
		{id: "203", title: "Investigate old flaky test", ageDays: 120},
		{id: "204", title: "Rewrite entire auth system end to end", ageDays: 5, oversized: true},
	}
}

// curate applies the same conservative-default decision shape described in the
// curator's instructions.md, deterministically, so the test is reproducible
// without an LLM in the loop.
func curateFixture(items []fixtureBacklogItem) map[string]interface{} {
	deduped, stale, split, ready := 0, 0, 0, 0
	const childrenPerSplit = 3
	for _, it := range items {
		switch {
		case it.duplicateOf != "":
			deduped++
		case it.oversized:
			split++
			ready += childrenPerSplit // children are ready; the parent becomes a tracking issue, not ready itself
		case it.ageDays > 90:
			stale++
		default:
			ready++
		}
	}
	return map[string]interface{}{
		"deduped":          deduped,
		"staleFlagged":     stale,
		"split":            split,
		"markedReady":      ready,
		"markedNeedsHuman": 0,
	}
}

func curationRunInput(spec apiv1.WorkflowSpec) RunInput {
	return RunInput{
		RunID:        "run-curation",
		Gaggle:       "acme-web",
		WorkflowName: "backlog-curation",
		Version:      1,
		Spec:         spec,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

// TestBacklogCurationDryRun exercises the shipped definition end to end over a
// fixture backlog containing a duplicate pair, a stale item, and an oversized
// epic — the three scenarios issue #25's acceptance criteria names.
func TestBacklogCurationDryRun(t *testing.T) {
	spec := loadCurationWorkflow(t)
	items := fixtureBacklog()

	var gotQueryInputs map[string]interface{}
	det := &fakeRunner{
		run: func(_ context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			gotQueryInputs = env.Inputs
			claimed := make([]interface{}, len(items))
			for i, it := range items {
				claimed[i] = it.id
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
				Outputs: map[string]interface{}{"curation-summary": curateFixture(items)},
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

	// curate ran second (goober = curator per the definition) and produced a
	// curation summary reflecting the fixture's decision shape: 1 dedupe pair,
	// 1 stale flag, 1 split (3 children), 4 total marked ready (1 survivor + 3
	// split children), 0 needing a human.
	if gotCurateGoal == "" {
		t.Error("curate stage did not receive a goal")
	}
	curateOut, ok := res.Outputs["curate"]
	if !ok || curateOut.Status != apiv1.ResultSuccess {
		t.Fatalf("curate output missing or not success: %+v", curateOut)
	}
	summary, ok := curateOut.Outputs["curation-summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("curation-summary missing or wrong shape: %#v", curateOut.Outputs["curation-summary"])
	}
	wantCounts := map[string]int{"deduped": 1, "staleFlagged": 1, "split": 1, "markedReady": 4, "markedNeedsHuman": 0}
	for k, want := range wantCounts {
		got, ok := summary[k]
		if !ok {
			t.Errorf("curation-summary missing %q", k)
			continue
		}
		gotInt, ok := toInt(got)
		if !ok || gotInt != want {
			t.Errorf("curation-summary[%q] = %v, want %d", k, got, want)
		}
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
				Outputs: map[string]interface{}{"curation-summary": curateFixture(nil)},
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
}

// toInt normalizes an activity-result numeric value: the Temporal test
// environment round-trips activity results through its data converter, so a Go
// int constructed in a fake may come back as float64.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
