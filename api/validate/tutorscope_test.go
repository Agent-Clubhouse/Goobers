package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tutorScopeConfig builds a single-gaggle Manifest+Gaggle+Goober plus a
// "target" workflow and a "tutor" workflow whose spec.tutorScope YAML block
// is substituted verbatim — the minimal shape TUT-A4's checkWorkflow logic
// inspects, without needing a dedicated testdata fixture per case.
func tutorScopeConfig(gaggle, tutorScopeYAML string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: tutor-scope
spec:
  instance:
    name: tutor-scope
    environment: dev
  gaggles:
    - ` + gaggle + `
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + gaggle + `
spec:
  project:
    provider: github
    owner: example
    name: ` + gaggle + `
  backlog:
    provider: github
    project: example/` + gaggle + `
  isolation:
    namespace: ` + gaggle + `
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: target
spec:
  gaggle: ` + gaggle + `
  triggers:
    - type: manual
  start: noop
  tasks:
    - name: noop
      type: deterministic
      goal: Do nothing.
      run:
        command: ["true"]
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: tutor
spec:
  gaggle: ` + gaggle + `
  triggers:
    - type: manual
  start: noop
` + tutorScopeYAML + `
  tasks:
    - name: noop
      type: deterministic
      goal: Do nothing.
      run:
        command: ["true"]
`
}

func validateTutorScope(t *testing.T, gaggle, tutorScopeYAML string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(tutorScopeConfig(gaggle, tutorScopeYAML)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func TestTutorScopePerWorkflowWithValidSameGaggleTargetValidatesClean(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-workflow\n    target: target\n")
	if report.HasErrors() {
		t.Fatalf("a per-workflow tutor scoped to a real same-gaggle target reported errors:\n%s", joinIssues(report))
	}
}

func TestTutorScopePerGaggleWithNoTargetValidatesClean(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-gaggle\n")
	if report.HasErrors() {
		t.Fatalf("a per-gaggle tutor with no target reported errors:\n%s", joinIssues(report))
	}
}

func TestTutorScopePerWorkflowRequiresTarget(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-workflow\n")
	if !report.HasErrors() {
		t.Fatal("expected a per-workflow tutor with no target to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `spec.tutorScope.target is required when spec.tutorScope.tier is "per-workflow"`) {
		t.Fatalf("missing target-required error:\n%s", got)
	}
}

func TestTutorScopePerWorkflowRejectsSelfTarget(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-workflow\n    target: tutor\n")
	if !report.HasErrors() {
		t.Fatal("expected a per-workflow tutor targeting itself to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `spec.tutorScope.target "tutor" must not name this workflow itself`) {
		t.Fatalf("missing self-target error:\n%s", got)
	}
}

func TestTutorScopePerWorkflowRejectsUndefinedTarget(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-workflow\n    target: nonexistent\n")
	if !report.HasErrors() {
		t.Fatal("expected a per-workflow tutor targeting an undefined workflow to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `spec.tutorScope.target names "nonexistent", but no Workflow/nonexistent definition was found in gaggle "alpha"`) {
		t.Fatalf("missing undefined-target error:\n%s", got)
	}
}

func TestTutorScopePerGaggleRejectsNonEmptyTarget(t *testing.T) {
	report := validateTutorScope(t, "alpha", "  tutorScope:\n    tier: per-gaggle\n    target: target\n")
	if !report.HasErrors() {
		t.Fatal("expected a per-gaggle tutor with a non-empty target to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `spec.tutorScope.target must be empty when spec.tutorScope.tier is "per-gaggle", got "target"`) {
		t.Fatalf("missing per-gaggle target-forbidden error:\n%s", got)
	}
}
