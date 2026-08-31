package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// personal-gaggle-routing §5.4 makes allowedRouteLabels (and the other inputs
// that bound the routing transaction) STATIC task configuration. The rejection
// has to be authoring-time: a task's inputsFrom values are merged into the same
// flat Inputs map the static ones occupy, so at runtime `backlog-query --route`
// cannot distinguish an author's declared allowlist from one a preceding
// agentic stage handed it. These tests pin the config validator's half of that
// guarantee — the compiler's half lives in
// internal/workflow/v_current/routeseparation_test.go.

const staticRouteManifest = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata: {name: inst}
spec:
  instance: {name: acme, environment: dev}
  gaggles: [router]
`

const staticRouteGaggle = `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata: {name: router}
spec:
  project: {provider: github, owner: acme, name: web}
  backlog: {provider: github, project: acme/private-backlog}
  isolation: {namespace: gaggle-router}
`

// staticRouteWorkflow builds a router workflow whose route stage optionally
// wires one of its inputs through inputsFrom from the preceding stage.
func staticRouteWorkflow(inputsFrom string) string {
	wiring := ""
	if inputsFrom != "" {
		wiring = "      inputsFrom:\n        " + inputsFrom + ": decision\n"
	}
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata: {name: router-routing}
spec:
  gaggle: router
  entry: decide-route
  tasks:
    - name: decide-route
      type: deterministic
      goal: Emit a route plan.
      run:
        command: ["goobers", "backlog-query", "--read-only"]
      inputs:
        resultFile: "route-plan.json"
      expectedOutputs: ["decision"]
      capabilities:
        - github:issues:read
      next: apply-route
    - name: apply-route
      type: deterministic
      goal: Apply routing labels and hand the item off.
      run:
        command: ["goobers", "backlog-query", "--route"]
      inputs:
        routePlanFile: "route-plan.json"
        resultFile: "route-result.json"
        allowedRouteLabels: "goobers:routed,repo:*"
        trustLabel: "goobers:route-approved"
` + wiring + `      capabilities:
        - github:issues:write
      policyActions:
        - route-backlog-item
`
}

func writeStaticRouteTree(t *testing.T, inputsFrom string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", staticRouteManifest)
	write("gaggles/router/gaggle.yaml", staticRouteGaggle)
	write("gaggles/router/workflows/routing.yaml", staticRouteWorkflow(inputsFrom))
	return dir
}

func staticRouteDiagnostics(t *testing.T, dir string) string {
	t.Helper()
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var found []string
	for _, issue := range report.Issues {
		if issue.Code == errorWorkflowAdmission && strings.Contains(issue.String(), "must declare input") {
			found = append(found, issue.String())
		}
	}
	return strings.Join(found, "\n")
}

// TestValidatorRejectsInputsFromOnStaticRouteInputs is the load-bearing case:
// a preceding stage that could supply allowedRouteLabels could widen the very
// allowlist that is supposed to bound it.
func TestValidatorRejectsInputsFromOnStaticRouteInputs(t *testing.T) {
	for _, input := range []string{"allowedRouteLabels", "routePlanFile", "trustLabel", "claimLabel"} {
		got := staticRouteDiagnostics(t, writeStaticRouteTree(t, input))
		if got == "" {
			t.Errorf("inputsFrom on %q must be reported by the config validator", input)
			continue
		}
		if !strings.Contains(got, input) {
			t.Errorf("diagnostic for %q should name the input, got:\n%s", input, got)
		}
	}
}

// TestValidatorAcceptsStaticallyDeclaredRouteInputs is the reference topology
// the feature exists to author: every bounding input declared statically on the
// route task itself. Rejecting this would make routing unauthorable.
func TestValidatorAcceptsStaticallyDeclaredRouteInputs(t *testing.T) {
	if got := staticRouteDiagnostics(t, writeStaticRouteTree(t, "")); got != "" {
		t.Fatalf("statically declared route inputs must be accepted, got:\n%s", got)
	}
}

// TestValidatorAcceptsInputsFromOnOrdinaryRouteInputs keeps the rejection
// scoped to the security contract rather than to inputsFrom in general.
func TestValidatorAcceptsInputsFromOnOrdinaryRouteInputs(t *testing.T) {
	if got := staticRouteDiagnostics(t, writeStaticRouteTree(t, "resultFile")); got != "" {
		t.Fatalf("a non-sensitive inputsFrom mapping must be accepted, got:\n%s", got)
	}
}
