package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflowcluster"
)

const workflowShowFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: prepare
  tasks:
    - name: prepare
      type: deterministic
      goal: Prepare the change.
      run:
        command: ["true"]
      next: review
    - name: finish
      type: agentic
      goober: coder
      goal: Finish the change.
      capabilities:
        - agent:model
  gates:
    - name: review
      evaluator: human
      human: {}
      branches:
        pass: finish
        fail: prepare
`

func TestWorkflowShowPrintsTextDAG(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowShowFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}

	want := `workflow: default-implement
triggers: manual-only
start: prepare
stages:
  prepare (kind: deterministic) -> review
  finish (kind: agentic) -> <complete>
  review (kind: gate, evaluator: human)
    pass target: finish
    fail target: prepare
`
	if stdout != want {
		t.Fatalf("workflow show stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestWorkflowShowSurfacesValidationWarnings(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowShowFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "has no schedule trigger; it will not fire autonomously") {
		t.Fatalf("workflow show stderr did not surface validation warning: %q", stderr)
	}
}

// workflowDOTFixture is workflowShowFixture's gate swapped from human to
// automated so the fixture remains focused on graph projection. It has the
// same node/edge topology (pass/fail branches to the same targets), so the
// expected DOT output is unaffected by the swap — DOT rendering has no
// evaluator-specific text.
const workflowDOTFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  start: prepare
  tasks:
    - name: prepare
      type: deterministic
      goal: Prepare the change.
      run:
        command: ["true"]
      next: review
    - name: finish
      type: agentic
      goober: coder
      goal: Finish the change.
      capabilities:
        - agent:model
  gates:
    - name: review
      evaluator: automated
      automated:
        check: tests-pass
      branches:
        pass: finish
        fail: prepare
`

func TestWorkflowShowPrintsDOT(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowDOTFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "--dot", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show --dot: code = %d, stderr = %q", code, stderr)
	}

	want := `digraph {
  "prepare" [shape=box];
  "prepare" -> "review";
  "finish" [shape=box];
  "finish" -> "<complete>";
  "review" [shape=diamond];
  "review" -> "finish" [label="pass"];
  "review" -> "prepare" [label="fail"];
}
`
	if stdout != want {
		t.Fatalf("workflow show --dot stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestWorkflowShowUnknownWorkflow(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "workflow", "show", "no-such-workflow", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `no workflow named "no-such-workflow"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestWorkflowUsage(t *testing.T) {
	code, _, stderr := runArgs(t, "workflow")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage: goobers workflow <show | validate-child | clusters> [flags]") {
		t.Fatalf("stderr = %q", stderr)
	}

	code, stdout, _ := runArgs(t, "workflow", "help")
	if code != 0 {
		t.Fatalf("workflow help code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Usage: goobers workflow <show | validate-child | clusters> [flags]") {
		t.Fatalf("workflow help stdout = %q", stdout)
	}

	code, _, stderr = runArgs(t, "workflow", "bogus")
	if code != 2 {
		t.Fatalf("unknown subcommand code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) {
		t.Fatalf("unknown subcommand stderr = %q", stderr)
	}

	code, stdout, _ = runArgs(t, "help")
	if code != 0 {
		t.Fatalf("help code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "\n  workflow show") {
		t.Fatalf("help stdout = %q", stdout)
	}
}

const workflowParallelFixture = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: query
  tasks:
    - name: query
      type: deterministic
      goal: Query work.
      run:
        command: ["true"]
      next: analyze
    - name: security
      type: deterministic
      goal: Analyze security.
      run:
        command: ["true"]
        workspace: scratch
      next: "@join"
    - name: performance
      type: deterministic
      goal: Analyze performance.
      run:
        command: ["true"]
        workspace: scratch
      next: "@join"
    - name: implement
      type: agentic
      goober: coder
      goal: Implement work.
      capabilities:
        - agent:model
  parallels:
    - name: analyze
      failurePolicy: all_or_nothing
      branches:
        - name: security
          start: security
        - name: performance
          start: performance
      join: implement
      onFailure: "@abort"
`

// TestWorkflowShowTextRendersParallels pins #2738: the text view lists the
// parallel with its branches and failure route, resolves each branch's
// "@join" to the join stage, and states the sequential default, agreeing
// with --dot rather than printing an edge into a state it never lists.
func TestWorkflowShowTextRendersParallels(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(workflowParallelFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show: code = %d, stderr = %q", code, stderr)
	}
	want := `workflow: default-implement
triggers: manual-only
start: query
stages:
  query (kind: deterministic) -> analyze
  security (kind: deterministic) -> implement
  performance (kind: deterministic) -> implement
  implement (kind: agentic) -> <complete>
  analyze (kind: parallel, maxConcurrentBranches unset: branches run sequentially)
    branch security start: security
    branch performance start: performance
    branch-failed target: @abort
`
	if stdout != want {
		t.Fatalf("workflow show stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if !strings.Contains(stderr, `parallel "analyze": maxConcurrentBranches is unset, so its 2 branches run sequentially`) {
		t.Fatalf("workflow show stderr did not surface the sequential-parallel warning: %q", stderr)
	}

	code, dot, stderr := runArgs(t, "workflow", "show", "--dot", "default-implement", root)
	if code != 0 {
		t.Fatalf("workflow show --dot: code = %d, stderr = %q", code, stderr)
	}
	for _, edge := range []string{
		`"analyze" -> "security";`,
		`"analyze" -> "performance";`,
		`"analyze" -> "@abort";`,
		`"security" -> "implement";`,
		`"performance" -> "implement";`,
	} {
		if !strings.Contains(dot, edge) {
			t.Fatalf("workflow show --dot missing %s:\n%s", edge, dot)
		}
	}
}

// TestWorkflowShowTextRefusesInvalidWorkflow is a regression guard for
// #2738: an invalid parallel workflow exits 1 instead of printing a confident
// DAG. Config validation rejects this case before show compiles; the compile
// step in show keeps the text view in lockstep with --dot if it ever does not.
func TestWorkflowShowTextRefusesInvalidWorkflow(t *testing.T) {
	root := initDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	broken := strings.Replace(workflowParallelFixture, "      join: implement\n", "      join: query\n", 1)
	if err := os.WriteFile(workflowPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "show", "default-implement", root)
	if code != 1 {
		t.Fatalf("workflow show: code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "stages:") {
		t.Fatalf("workflow show printed a DAG for an invalid workflow:\n%s", stdout)
	}
}

func TestParallelConcurrencyDescribesEachSetting(t *testing.T) {
	for _, tc := range []struct {
		limit int32
		want  string
	}{
		{0, "maxConcurrentBranches unset: branches run sequentially"},
		{1, "maxConcurrentBranches: 1, branches run sequentially"},
		{3, "maxConcurrentBranches: 3"},
	} {
		parallels := []apiv1.Parallel{{Name: "other", MaxConcurrentBranches: 4}, {Name: "fan", MaxConcurrentBranches: tc.limit}}
		if got := parallelConcurrency(parallels, "fan"); got != tc.want {
			t.Fatalf("limit %d: got %q, want %q", tc.limit, got, tc.want)
		}
	}
}

func TestWorkflowClustersReportsConfiguredWorkflows(t *testing.T) {
	root := initDemo(t)
	workflowDir := filepath.Join(root, "config", "gaggles", "example", "workflows")
	raw, err := os.ReadFile(filepath.Join(workflowDir, "default-implement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sibling := strings.Replace(string(raw), "name: default-implement", "name: sibling-implement", 1)
	if sibling == string(raw) {
		t.Fatal("workflow fixture metadata name was not replaced")
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "sibling-implement.yaml"), []byte(sibling), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", "--json", root)
	if code != 0 {
		t.Fatalf("workflow clusters: code = %d, stderr = %q", code, stderr)
	}
	var model struct {
		Version, FeatureVersion string
		Population              int
		Clusters                []struct{ Members []string }
		Noise                   []string
	}
	if err := json.Unmarshal([]byte(stdout), &model); err != nil {
		t.Fatalf("decode model: %v\n%s", err, stdout)
	}
	assigned := len(model.Noise)
	for _, c := range model.Clusters {
		assigned += len(c.Members)
	}
	if model.Version != "workflowcluster.model/v1" || model.Population < 2 || assigned != model.Population {
		t.Fatalf("model = %+v", model)
	}

	code, stdout, stderr = runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", root)
	if code != 0 || !strings.Contains(stdout, "Labs: experimental") {
		t.Fatalf("text report: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if code, _, _ := runArgs(t, "workflow", "clusters", "--min-cluster-size", "1", root); code != 2 {
		t.Fatalf("min-cluster-size 1: code = %d, want 2", code)
	}
}

// TestLabsWorkflowClustersViewMatchesConfiguredInstance pins the daemon's
// Labs cluster read (#8309) to the same configured data the CLI clusters.
func TestLabsWorkflowClustersViewMatchesConfiguredInstance(t *testing.T) {
	root := initDemo(t)
	source := workflowcluster.ConfiguredSource{Root: root}
	view, err := source.WorkflowClusters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Population >= view.MinClusterSize || view.Status != workflowcluster.ViewInsufficientData || view.Model != nil {
		t.Fatalf("demo view = %+v, want insufficient data", view)
	}
	workflowDir := filepath.Join(root, "config", "gaggles", "example", "workflows")
	raw, err := os.ReadFile(filepath.Join(workflowDir, "default-implement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sibling-a", "sibling-b", "sibling-c"} {
		sibling := strings.Replace(string(raw), "name: default-implement", "name: "+name, 1)
		if err := os.WriteFile(filepath.Join(workflowDir, name+".yaml"), []byte(sibling), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	view, err = source.WorkflowClusters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != workflowcluster.ViewClustered || view.Model == nil || view.Confounds == nil || view.Model.Population != view.Population {
		t.Fatalf("view = %+v, want a clustered model with confounds", view)
	}
	assigned := len(view.Model.Noise)
	for _, c := range view.Model.Clusters {
		assigned += len(c.Members)
	}
	if assigned != view.Population || view.Confounds.Evidence != workflowcluster.EvidenceObservational {
		t.Fatalf("model = %+v confounds = %+v", view.Model, view.Confounds)
	}
	if view.Trends == nil || view.TrendsUnavailable != "" {
		t.Fatalf("trends = %+v, unavailable = %q", view.Trends, view.TrendsUnavailable)
	}

	// An unreadable journal root degrades only the supplementary trends.
	legacyRuns := instance.NewLayout(root).RunsDir()
	if err := os.RemoveAll(legacyRuns); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyRuns, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	degraded, err := source.WorkflowClusters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if degraded.Status != workflowcluster.ViewClustered || degraded.Model == nil || degraded.Trends != nil ||
		degraded.TrendsUnavailable == "" || degraded.TrendsErr == nil {
		t.Fatalf("degraded view = %+v", degraded)
	}
}

func TestWorkflowClustersStratifyReportsConfounds(t *testing.T) {
	root := initDemo(t)
	workflowDir := filepath.Join(root, "config", "gaggles", "example", "workflows")
	raw, err := os.ReadFile(filepath.Join(workflowDir, "default-implement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sibling := strings.Replace(string(raw), "name: default-implement", "name: sibling-implement", 1)
	if err := os.WriteFile(filepath.Join(workflowDir, "sibling-implement.yaml"), []byte(sibling), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", "--stratify", "gaggle", "--json", root)
	if code != 0 {
		t.Fatalf("workflow clusters --stratify: code = %d, stderr = %q", code, stderr)
	}
	var out struct {
		Model struct {
			Population int
		}
		Confounds struct {
			Evidence   string
			Strata     []struct{ Key string }
			Undersized []string
		}
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if out.Model.Population < 2 || out.Confounds.Evidence != "observational" ||
		len(out.Confounds.Strata) == 0 || len(out.Confounds.Undersized) == 0 {
		t.Fatalf("output = %+v", out)
	}

	code, stdout, stderr = runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", "--stratify", "gaggle", root)
	if code != 0 || !strings.Contains(stdout, "Confounds (stratified by gaggle; evidence: observational)") ||
		!strings.Contains(stdout, "reported, not pooled") {
		t.Fatalf("text report: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	for _, axis := range []string{"cohort", "model", "harness", "skills", "tools"} {
		code, stdout, stderr := runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", "--stratify", axis, "--json", root)
		if code != 0 {
			t.Fatalf("stratify %s: code = %d, stderr = %q", axis, code, stderr)
		}
		var got struct {
			Model     struct{ Population int }
			Confounds *struct {
				Evidence  string
				Unlabeled int
				Strata    []struct{ Size int }
			}
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stratify %s decode: %v\n%s", axis, err, stdout)
		}
		if got.Confounds == nil || got.Confounds.Evidence != "observational" {
			t.Fatalf("stratify %s: confounds = %+v", axis, got.Confounds)
		}
		labeled := 0
		for _, s := range got.Confounds.Strata {
			labeled += s.Size
		}
		if labeled+got.Confounds.Unlabeled != got.Model.Population {
			t.Fatalf("stratify %s: labeled %d + unlabeled %d != population %d", axis, labeled, got.Confounds.Unlabeled, got.Model.Population)
		}
		if axis == "cohort" && got.Confounds.Unlabeled != 0 {
			t.Fatalf("stratify cohort: every configured workflow has a task cohort, got %d unlabeled", got.Confounds.Unlabeled)
		}
		code, stdout, _ = runArgs(t, "workflow", "clusters", "--min-cluster-size", "2", "--stratify", axis, root)
		if code != 0 || !strings.Contains(stdout, "Confounds (stratified by "+axis+"; evidence: observational)") ||
			!strings.Contains(stdout, "unlabeled)") {
			t.Fatalf("stratify %s text: code = %d, stdout = %q", axis, code, stdout)
		}
	}
	if code, _, _ := runArgs(t, "workflow", "clusters", "--stratify", "bogus", root); code != 2 {
		t.Fatalf("stratify bogus: code = %d, want 2", code)
	}
}
