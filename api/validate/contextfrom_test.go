package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contextFromConfig builds a two-task workflow whose second task's contextFrom
// list is the caller's to vary.
func contextFromConfig(contextFromYAML string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: context-from
spec:
  instance:
    name: context-from
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
    name: web
  backlog:
    provider: github
    project: example/web
  isolation:
    namespace: web
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: context-flow
spec:
  gaggle: web
  triggers:
    - type: manual
  start: first
  tasks:
    - name: first
      type: deterministic
      goal: produce an artifact
      run:
        command: ["goobers", "noop"]
      next: second
    - name: second
      type: deterministic
      goal: consume the artifact
      run:
        command: ["goobers", "noop"]
` + contextFromYAML
}

func validateContextFrom(t *testing.T, contextFromYAML string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(contextFromConfig(contextFromYAML)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

// TestContextFromDuplicateRejected covers the constraint that moved off the Go
// type: it used to be +kubebuilder:validation:UniqueItems=true, which made the
// generated CRD un-installable because Kubernetes forbids uniqueItems in a
// structural schema. The guarantee now lives here, so it has to be tested here.
func TestContextFromDuplicateRejected(t *testing.T) {
	report := validateContextFrom(t, `      contextFrom:
        - first
        - first
`)
	got := joinIssues(report)
	if !strings.Contains(got, "CTX001") {
		t.Fatalf("duplicate contextFrom entry did not report CTX001:\n%s", got)
	}
	if !strings.Contains(got, `spec.tasks[1].contextFrom lists "first" more than once`) {
		t.Fatalf("CTX001 message did not name the task index and duplicate:\n%s", got)
	}
}

func TestContextFromDistinctEntriesClean(t *testing.T) {
	report := validateContextFrom(t, `      contextFrom:
        - first
`)
	if got := joinIssues(report); strings.Contains(got, "CTX001") {
		t.Fatalf("a distinct contextFrom list reported CTX001:\n%s", got)
	}
}

// TestContextFromEmptyClean guards the documented default: an empty list means
// "receive every accumulated pointer", so it must not trip the uniqueness check.
func TestContextFromEmptyClean(t *testing.T) {
	report := validateContextFrom(t, "")
	if got := joinIssues(report); strings.Contains(got, "CTX001") {
		t.Fatalf("an omitted contextFrom reported CTX001:\n%s", got)
	}
}
