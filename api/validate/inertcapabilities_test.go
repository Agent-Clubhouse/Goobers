package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inertADOCapabilityConfig is an Azure DevOps gaggle whose workflow, pinned
// to dslVersion, declares every ado:* capability on some stage.
func inertADOCapabilityConfig(dslVersion string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: inert-ado
spec:
  instance:
    name: inert-ado
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
    provider: ado
    owner: example-org
    project: example-project
    name: web
  backlog:
    provider: ado
    project: example-project
  isolation:
    namespace: web
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: reviewer
spec:
  gaggle: web
  role: reviewer
  capabilities:
    - ado:pr:write
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: bystander
spec:
  gaggle: web
  role: reviewer
  capabilities:
    - ado:code:read
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "` + dslVersion + `"
metadata:
  name: inert-flow
spec:
  gaggle: web
  triggers:
    - type: manual
  start: comment
  tasks:
    - name: comment
      type: deterministic
      goal: post a status
      run:
        command: ["goobers", "report-pr-status"]
      capabilities: ["github:pr:write", "ado:pr:comment"]
      next: open
    - name: open
      type: deterministic
      goal: open the pull request and link its work item
      run:
        command: ["goobers", "open-pr"]
      capabilities: ["provider:pr:write", "ado:work-items:write"]
      next: triage
    - name: triage
      type: deterministic
      goal: update the work item
      run:
        command: ["goobers", "issue-close-out"]
      capabilities: ["github:issues:write", "ado:work-items:write", "ado:code:read"]
      next: land
    - name: land
      type: deterministic
      goal: land the pull request
      run:
        command: ["goobers", "merge-pr"]
      capabilities: ["github:pr:merge", "github:branch:delete", "ado:pr:complete", "ado:pr:status"]
      next: review
    - name: review
      type: agentic
      goal: review the change
      goober: reviewer
`
}

func validateInertADOCapabilities(t *testing.T, dslVersion string) []Issue {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(inertADOCapabilityConfig(dslVersion)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var inert []Issue
	for _, issue := range report.Issues {
		if issue.Code == WarningInertADOCapability {
			inert = append(inert, issue)
		}
	}
	return inert
}

// TestInertADOCapabilityWarnsWithAuthorizingName covers the ADO-N24 advisory
// (docs/design/ado-parity-dsl-2-0.md §3.1): each inert ado:* name draws a
// warning naming what authorizes the operation, while honoured names
// (ado:pr:complete, ado:pr:status, and ado:work-items:write on open-pr) and
// goobers no DSL 2.0 stage runs draw none.
func TestInertADOCapabilityWarnsWithAuthorizingName(t *testing.T) {
	issues := validateInertADOCapabilities(t, "2.0")
	want := []struct{ kind, name, fragments string }{
		{"Workflow", "inert-flow", `task "comment" declares capability "ado:pr:comment"|"github:pr:write" authorizes`},
		{"Workflow", "inert-flow", `task "triage" declares capability "ado:work-items:write"|"github:issues:write" authorizes`},
		{"Workflow", "inert-flow", `task "triage" declares capability "ado:code:read"|no capability is needed: repository reads use the repository credential`},
		{"Goober", "reviewer", `grants "ado:pr:write"|"github:pr:write" authorizes`},
	}
	if len(issues) != len(want) {
		t.Fatalf("got %d CAP006 findings, want %d:\n%v", len(issues), len(want), issues)
	}
	for _, expected := range want {
		found := false
		for _, issue := range issues {
			if issue.Kind != expected.kind || issue.Name != expected.name {
				continue
			}
			matches := true
			for _, fragment := range strings.Split(expected.fragments, "|") {
				matches = matches && strings.Contains(issue.Message, fragment)
			}
			found = found || matches
		}
		if !found {
			t.Errorf("no CAP006 %s/%s finding containing %q in:\n%v", expected.kind, expected.name, expected.fragments, issues)
		}
	}
	for _, issue := range issues {
		if issue.Severity != Warning {
			t.Errorf("CAP006 severity = %s, want warning: %v", issue.Severity, issue)
		}
		for _, honoured := range []string{"ado:pr:complete", "ado:pr:status", `task "open"`} {
			if strings.Contains(issue.Message, honoured) {
				t.Errorf("CAP006 reported an honoured declaration (%s): %v", honoured, issue)
			}
		}
	}
}

// TestInertADOCapabilityScopedToDSL2 keeps the advisory to DSL 2.0: the
// provider access layer design owns DSL 3.0's capability vocabulary.
func TestInertADOCapabilityScopedToDSL2(t *testing.T) {
	if issues := validateInertADOCapabilities(t, "3.0"); len(issues) != 0 {
		t.Fatalf("a DSL 3.0 workflow reported CAP006:\n%v", issues)
	}
}
