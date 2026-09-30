package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnersolve"
)

func TestRuntimePreflightJSONReportContract(t *testing.T) {
	root := writeRuntimePreflightFixture(t)
	t.Setenv("GOOBERS_COPILOT_TOKEN", "super-secret-token")

	var stdout, stderr bytes.Buffer
	code := runRuntimePreflight([]string{
		"--instance", root,
		"--workflow", "implement",
		"--execution-identity", "actual",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runRuntimePreflight exit %d, stderr:\n%s", code, stderr.String())
	}
	output := stdout.String()
	for _, leaked := range []string{"super-secret-token", "SECRET_FROM_INSTRUCTIONS", "GOOBERS_COPILOT_TOKEN"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("JSON report leaked %q:\n%s", leaked, output)
		}
	}

	var report runtimePreflightReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, output)
	}
	if report.SchemaVersion != runtimePreflightSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", report.SchemaVersion, runtimePreflightSchemaVersion)
	}
	if report.Workflow.Name != "implement" || report.Workflow.Gaggle != "example" {
		t.Fatalf("workflow identity = %s/%s, want example/implement", report.Workflow.Gaggle, report.Workflow.Name)
	}
	if !strings.HasPrefix(report.Workflow.Digest, "sha256:") || !strings.HasPrefix(report.Workflow.GooberDigest, "sha256:") {
		t.Fatalf("digests = workflow %q goober %q, want sha256 pins", report.Workflow.Digest, report.Workflow.GooberDigest)
	}
	if report.Execution.IdentityMode != "actual" || report.Execution.Source.Fidelity != "static" {
		t.Fatalf("execution = %+v, want actual/static", report.Execution)
	}
	if report.Execution.Runner.Name != "local" || report.Execution.Runner.Kind != "self" || !containsString(report.Execution.Runner.Capabilities, "node@20") {
		t.Fatalf("runner = %+v, want statically known host:self runner named local", report.Execution.Runner)
	}
	if len(report.MutationBoundary) == 0 || !containsString(report.MutationBoundary, "no repository write") || !containsString(report.MutationBoundary, "no model execution") {
		t.Fatalf("mutation boundary = %v, want explicit no-write/no-model entries", report.MutationBoundary)
	}

	stageByName := map[string]runtimePreflightStage{}
	for _, stage := range report.Stages {
		stageByName[stage.Name] = stage
	}
	claim := stageByName["claim"]
	if claim.Kind != "task:deterministic" || !containsString(claim.CredentialCapabilities, "github:issues:write") || !containsString(claim.RequiredCapabilities, "node@20") {
		t.Fatalf("claim stage = %+v, want deterministic capabilities", claim)
	}
	implement := stageByName["implement"]
	if implement.Kind != "task:agentic" || implement.Goober != "coder" || implement.Harness != "copilot" || !containsString(implement.CredentialCapabilities, "agent:model") {
		t.Fatalf("implement stage = %+v, want agentic goober/harness/capabilities", implement)
	}
	if !containsString(implement.RequiredCapabilities, "node@20") {
		t.Fatalf("implement required capabilities = %v, want gaggle-level node@20 floor", implement.RequiredCapabilities)
	}

	wantCategories := []string{
		"authentication",
		"transport",
		"authorization",
		"unavailable_tool",
		"path_access",
		"sandbox",
		"capability_mismatch",
		"cleanup_guarantee",
		"unsupported",
		"unobservable",
	}
	for _, category := range wantCategories {
		check, ok := findRuntimePreflightCheck(report.Checks, category)
		if !ok {
			t.Fatalf("missing check category %q in %+v", category, report.Checks)
		}
		if check.Outcome != "unobservable" && check.Outcome != "unsupported" {
			t.Fatalf("check %q outcome = %q, want explicit unsupported/unobservable", category, check.Outcome)
		}
	}
}

func TestRuntimePreflightHumanReportRedactsSecrets(t *testing.T) {
	root := writeRuntimePreflightFixture(t)
	t.Setenv("GOOBERS_COPILOT_TOKEN", "super-secret-token")

	var stdout, stderr bytes.Buffer
	code := runRuntimePreflight([]string{
		"--instance", root,
		"--workflow", "implement",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runRuntimePreflight exit %d, stderr:\n%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"runtime preflight: workflow implement", "workflow digest: sha256:", "goober digest: sha256:", "no external probes or mutations performed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("human report missing %q:\n%s", want, output)
		}
	}
	for _, leaked := range []string{"super-secret-token", "SECRET_FROM_INSTRUCTIONS", "GOOBERS_COPILOT_TOKEN"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("human report leaked %q:\n%s", leaked, output)
		}
	}
}

func TestRuntimePreflightStagesIncludesGatePlacementRequirements(t *testing.T) {
	stages := runtimePreflightStages(
		apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Gates: []apiv1.Gate{{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
			}},
		}},
		map[string]apiv1.GooberSpec{"reviewer": {Harness: apiv1.HarnessCopilot}},
		[]runnersolve.StageRequirement{{
			Stage:        "review",
			Capabilities: []string{"node@20", "harness:copilot"},
		}},
	)
	if len(stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(stages))
	}
	if !containsString(stages[0].RequiredCapabilities, "node@20") || !containsString(stages[0].RequiredCapabilities, "harness:copilot") {
		t.Fatalf("gate required capabilities = %v, want effective placement requirements", stages[0].RequiredCapabilities)
	}
}

func TestPreflightDispatcherKeepsWSLPathUnlessRuntimeFlagsPresent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runOnboardingPreflightWith(nil, &stdout, &stderr, wslPreflightDeps{
		hostOS: "plan9",
	})
	if code != 2 {
		t.Fatalf("legacy preflight exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "WSL preflight is only available") {
		t.Fatalf("legacy stderr = %q, want WSL preflight path", stderr.String())
	}

	if isRuntimePreflightInvocation([]string{"--launch-wsl", "--", "run", "implement", ".", "--json"}) {
		t.Fatal("forwarded --json after -- was classified as runtime preflight")
	}
}

func findRuntimePreflightCheck(checks []runtimePreflightCheck, category string) (runtimePreflightCheck, bool) {
	for _, check := range checks {
		if check.Category == category {
			return check, true
		}
	}
	return runtimePreflightCheck{}, false
}

func writeRuntimePreflightFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRuntimePreflightFile(t, root, "instance.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos:
  - provider: github
    owner: example
    name: repo
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN
runners:
  - name: local
    host: self
    provides:
      capabilities:
        - node@20
`)
	writeRuntimePreflightFile(t, root, filepath.Join("config", "manifest.yaml"), `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: example
spec:
  instance:
    name: example
    environment: dev
  gaggles:
    - example
`)
	writeRuntimePreflightFile(t, root, filepath.Join("config", "gaggles", "example", "gaggle.yaml"), `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project:
    provider: github
    owner: example
    name: repo
    branch: main
  backlog:
    provider: github
    project: example/repo
    labels:
      - goobers
  isolation:
    namespace: gaggle-example
    identityRef: example-identity
  requiredCapabilities:
    - node@20
`)
	writeRuntimePreflightFile(t, root, filepath.Join("config", "gaggles", "example", "goobers", "coder", "goober.yaml"), `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: coder
spec:
  gaggle: example
  role: coder
  instructions: instructions.md
  harness: copilot
  model: auto
  capabilities:
    - agent:model
  workflows:
    - implement
`)
	writeRuntimePreflightFile(t, root, filepath.Join("config", "gaggles", "example", "goobers", "coder", "instructions.md"), `Do not leak SECRET_FROM_INSTRUCTIONS.
`)
	writeRuntimePreflightFile(t, root, filepath.Join("config", "gaggles", "example", "workflows", "implement.yaml"), `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: implement
spec:
  gaggle: example
  triggers:
    - type: manual
  start: claim
  tasks:
    - name: claim
      type: deterministic
      goal: Claim work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        resultFile: claimed-item.json
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
      requiredCapabilities:
        - node@20
      next: implement
    - name: implement
      type: agentic
      goober: coder
      workspace: repo
      goal: Implement the claimed work.
      capabilities:
        - agent:model
`)
	return root
}

func writeRuntimePreflightFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
