package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prLifecycleBaseConfig builds a single-gaggle Manifest+Gaggle+Workflow with
// one goobers-CLI task, for #2088's checkPRLifecycleBaseBranch. gaggleBranch
// sets the gaggle's spec.project.branch ("" omits the field, matching a
// gaggle that leaves it unset). taskYAML is substituted as the task's own
// body (name/type/goal/run are fixed; inputs/inputsFrom are the caller's to
// vary).
func prLifecycleBaseConfig(gaggleBranch, taskYAML string) string {
	branchLine := ""
	if gaggleBranch != "" {
		branchLine = "\n    branch: " + gaggleBranch
	}
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: pr-lifecycle-base
spec:
  instance:
    name: pr-lifecycle-base
    environment: dev
  gaggles:
    - web
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project:
    provider: github
    owner: example
    name: web` + branchLine + `
  backlog:
    provider: github
    project: example/web
  isolation:
    namespace: web
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: pr-flow
spec:
  gaggle: web
  triggers:
    - type: manual
  start: select
  tasks:
` + taskYAML + `
`
}

func validatePRLifecycleBase(t *testing.T, gaggleBranch, taskYAML string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(prLifecycleBaseConfig(gaggleBranch, taskYAML)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func TestPRLifecycleBaseMatchesGaggleBranchValidatesClean(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: select the PR
      run:
        command: ["goobers", "pr-select"]
      inputs:
        base: release
      capabilities:
        - github:pr:write
      policyActions:
        - flag-foundation-coupling
`
	report := validatePRLifecycleBase(t, "release", task)
	if report.HasErrors() {
		t.Fatalf("a base matching the gaggle's own branch reported errors:\n%s", joinIssues(report))
	}
	if got := joinIssues(report); strings.Contains(got, "PRB001") || strings.Contains(got, "PRB002") {
		t.Fatalf("expected no PRB warnings, got:\n%s", got)
	}
}

func TestPRLifecycleBaseDriftFlagged(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: select the PR
      run:
        command: ["goobers", "pr-select"]
      inputs:
        base: main
`
	report := validatePRLifecycleBase(t, "release", task)
	got := joinIssues(report)
	if !strings.Contains(got, `task "select" declares base "main", but gaggle "web" resolves to branch "release"`) {
		t.Fatalf("missing base-drift warning:\n%s", got)
	}
}

func TestPRLifecycleBaseOmittedOnNonMainBranchFlagged(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: select the PR
      run:
        command: ["goobers", "pr-select"]
`
	report := validatePRLifecycleBase(t, "release", task)
	got := joinIssues(report)
	if !strings.Contains(got, `task "select" declares no base input; gaggle "web"'s branch is "release"`) {
		t.Fatalf("missing base-omitted warning:\n%s", got)
	}
}

func TestPRLifecycleBaseOmittedOnDefaultMainBranchValidatesClean(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: select the PR
      run:
        command: ["goobers", "pr-select"]
`
	// No branch set at all — resolves to "main" by RepoRef's own default, so
	// omitting base is correct and unremarkable, not just "same as main".
	report := validatePRLifecycleBase(t, "", task)
	if got := joinIssues(report); strings.Contains(got, "PRB001") || strings.Contains(got, "PRB002") {
		t.Fatalf("expected no PRB warnings for a default-branch gaggle omitting base, got:\n%s", got)
	}
}

func TestPRLifecycleBaseDynamicInputsFromSkipped(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: select the PR
      run:
        command: ["goobers", "pr-select"]
      inputsFrom:
        base: upstream.base
`
	report := validatePRLifecycleBase(t, "release", task)
	if got := joinIssues(report); strings.Contains(got, "PRB001") || strings.Contains(got, "PRB002") {
		t.Fatalf("a dynamic (inputsFrom) base should never be statically flagged, got:\n%s", got)
	}
}

func TestPRLifecycleBaseIgnoresNonPRLifecycleCommand(t *testing.T) {
	task := `    - name: select
      type: deterministic
      goal: query the backlog
      run:
        command: ["goobers", "backlog-query"]
`
	report := validatePRLifecycleBase(t, "release", task)
	if got := joinIssues(report); strings.Contains(got, "PRB001") || strings.Contains(got, "PRB002") {
		t.Fatalf("a non-PR-lifecycle command should never be flagged, got:\n%s", got)
	}
}
