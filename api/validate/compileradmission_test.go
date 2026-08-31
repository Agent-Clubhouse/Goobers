package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compilerAdmissionConfig builds a config directory whose two-task workflow
// compiles cleanly, with the tail of the second task's declaration (its
// contextFrom list, and any further states) left to the caller to vary.
func compilerAdmissionConfig(tail string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: compiler-admission
spec:
  instance:
    name: compiler-admission
    environment: dev
  gaggles:
    - site
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: site
spec:
  project:
    provider: github
    owner: example
    name: site
  backlog:
    provider: github
    project: example/site
  isolation:
    namespace: site
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: author
spec:
  gaggle: site
  role: coder
  instructions: config.yaml
  capabilities:
    - agent:model
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: publish
spec:
  gaggle: site
  triggers:
    - type: manual
  start: draft
  tasks:
    - name: draft
      type: agentic
      goober: author
      goal: Draft the site update.
      capabilities:
        - agent:model
      next: revise
    - name: revise
      type: agentic
      goober: author
      goal: Revise the draft.
      capabilities:
        - agent:model
` + tail
}

func validateCompilerAdmission(t *testing.T, tail string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(compilerAdmissionConfig(tail)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

// TestUnresolvedContextFromRejected is the #3664 regression: canonical config
// loading used to admit a contextFrom source naming no state at all, leaving
// the refusal to whoever later called workflow.Compile — so `goobers validate`
// and daemon startup rejected the definition while a GitOps or config-sync
// consumer of the loader accepted it.
func TestUnresolvedContextFromRejected(t *testing.T) {
	report := validateCompilerAdmission(t, `      contextFrom:
        - nowhere
`)
	got := joinIssues(report)
	if !strings.Contains(got, "WF025") {
		t.Fatalf("an unresolved contextFrom source did not report WF025:\n%s", got)
	}
	if !strings.Contains(got, `contextFrom source "nowhere" is not a defined task or gate`) {
		t.Fatalf("WF025 message did not name the unresolved source:\n%s", got)
	}
	if !report.HasErrors() {
		t.Fatalf("an unresolved contextFrom source did not fail validation:\n%s", got)
	}
}

// TestCompilerOnlyTopologyRuleRejected covers the second half of the same gap:
// a rule that lives only in the versioned compiler, with no mirror among the
// field-by-field checks. A dotted state name breaks qualified inputsFrom
// references, and nothing outside Compile said so.
func TestCompilerOnlyTopologyRuleRejected(t *testing.T) {
	report := validateCompilerAdmission(t, `      contextFrom:
        - draft
      next: publish.result
    - name: publish.result
      type: agentic
      goober: author
      goal: Publish the revision.
      capabilities:
        - agent:model
`)
	got := joinIssues(report)
	if !strings.Contains(got, "WF025") {
		t.Fatalf("a dotted state name did not report WF025:\n%s", got)
	}
	if !strings.Contains(got, `task name "publish.result" contains a dot`) {
		t.Fatalf("WF025 message did not name the dotted state:\n%s", got)
	}
}

// TestCompilerAdmissionCleanOnValidConfig is the no-false-positive control: a
// definition the compiler accepts must not gain a WF025 finding, including on
// the shipped example tree the loader's own tests treat as canonical.
func TestCompilerAdmissionCleanOnValidConfig(t *testing.T) {
	report := validateCompilerAdmission(t, `      contextFrom:
        - draft
`)
	if got := joinIssues(report); strings.Contains(got, "WF025") {
		t.Fatalf("a compilable workflow reported WF025:\n%s", got)
	}
	if report.HasErrors() {
		t.Fatalf("a compilable config failed validation:\n%s", joinIssues(report))
	}

	examples, err := newV(t).ValidateDir("../../config-examples")
	if err != nil {
		t.Fatalf("ValidateDir(config-examples): %v", err)
	}
	if got := joinIssues(examples); strings.Contains(got, "WF025") {
		t.Fatalf("the shipped config examples reported WF025:\n%s", got)
	}
}

// TestCompilerAdmissionDefersToPreciseFindings keeps each defect reported once:
// the compiler aggregates every problem in a document, so on a config the
// field-by-field checks already rejected it must stay quiet rather than
// restate their findings — and findings that belong to a Goober or Gaggle —
// under a second, less precise code.
func TestCompilerAdmissionDefersToPreciseFindings(t *testing.T) {
	report := validateCompilerAdmission(t, `      contextFrom:
        - draft
        - draft
`)
	got := joinIssues(report)
	if !strings.Contains(got, "CTX001") {
		t.Fatalf("duplicate contextFrom entry did not report CTX001:\n%s", got)
	}
	if strings.Contains(got, "WF025") {
		t.Fatalf("compiler admission restated an already-reported defect:\n%s", got)
	}
}
