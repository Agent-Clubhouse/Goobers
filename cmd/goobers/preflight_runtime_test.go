package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
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
	assertSelectedRunner(t, claim, "linux-pool", "image")
	implement := stageByName["implement"]
	if implement.Kind != "task:agentic" || implement.Goober != "coder" || implement.Harness != "copilot" || !containsString(implement.CredentialCapabilities, "agent:model") {
		t.Fatalf("implement stage = %+v, want agentic goober/harness/capabilities", implement)
	}
	if !containsString(implement.RequiredCapabilities, "node@20") {
		t.Fatalf("implement required capabilities = %v, want gaggle-level node@20 floor", implement.RequiredCapabilities)
	}
	assertSelectedRunner(t, implement, "linux-pool", "image")

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

	gotGolden := normalizedRuntimePreflightJSON(t, report)
	goldenPath := filepath.Join("testdata", "preflight_runtime_report.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, gotGolden, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	wantGolden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(gotGolden) != string(wantGolden) {
		t.Fatalf("preflight JSON golden mismatch (-want +got)\nwant:\n%s\n\ngot:\n%s", string(wantGolden), string(gotGolden))
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
	for _, want := range []string{
		"runtime preflight: workflow implement",
		"workflow digest: sha256:",
		"goober digest: sha256:",
		"runner: selected selected=linux-pool kind=image",
		"authentication/authentication_unobservable: unobservable",
		"cleanup_guarantee/cleanup_guarantee_unsupported: unsupported",
		"capability_mismatch/stage_capability_satisfaction_unobservable: unobservable stage=claim",
		"no external probes or mutations performed",
	} {
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

func TestRuntimePreflightLocalModeRunnerUnobservable(t *testing.T) {
	stages := runtimePreflightStages(
		apiv1.Workflow{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "plain", Type: apiv1.TaskDeterministic}}}},
		nil,
		[]runnersolve.StageRequirement{{Stage: "plain"}},
		map[string]runtimePreflightStageRunner{"plain": runtimePreflightRunnerUnobservable("local mode")},
	)
	if len(stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(stages))
	}
	if stages[0].Runner.Outcome != "unobservable" || stages[0].Runner.Source.Fidelity != "unobservable" {
		t.Fatalf("runner fact = %+v, want explicit unobservable", stages[0].Runner)
	}
}

func TestRuntimePreflightControlPlanePlacementUnobservable(t *testing.T) {
	cfg := &instance.Config{Runners: []instance.RunnerEntry{{
		Name: "local",
		Host: instance.RunnerHostSelfName,
		Provides: instance.RunnerProvides{
			Capabilities: []string{"node@20"},
		},
	}, {
		Name: "linux-pool",
		Host: "ghcr.io/example/goobers-runner:latest",
		Provides: instance.RunnerProvides{
			OS:           instance.RunnerOSLinux,
			Capabilities: []string{"node@20"},
		},
	}}}
	requirements := []runnersolve.StageRequirement{{
		Stage:        "review",
		ControlPlane: true,
		Capabilities: []string{"node@20"},
	}}
	facts, err := runtimePreflightStageRunnerFacts(
		cfg,
		workflow.Definition{Name: "implement", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{Name: "review"}}}},
		requirements,
		cfg.PlacementInventory(runnersolve.HostOS()),
	)
	if err != nil {
		t.Fatalf("runtimePreflightStageRunnerFacts: %v", err)
	}
	if facts["review"].Outcome != "unobservable" || !strings.Contains(facts["review"].Detail, "control-plane") {
		t.Fatalf("control-plane runner fact = %+v, want unobservable", facts["review"])
	}
}

func TestRuntimePreflightControlPlaneUnsatRemainsUnsupported(t *testing.T) {
	cfg := &instance.Config{Runners: []instance.RunnerEntry{{
		Name: "local",
		Host: instance.RunnerHostSelfName,
		Provides: instance.RunnerProvides{
			Capabilities: []string{"node@20"},
		},
	}, {
		Name: "linux-pool",
		Host: "ghcr.io/example/goobers-runner:latest",
		Provides: instance.RunnerProvides{
			OS:           instance.RunnerOSLinux,
			Capabilities: []string{"node@20"},
		},
	}}}
	requirements := []runnersolve.StageRequirement{{
		Stage:        "review",
		ControlPlane: true,
		Capabilities: []string{"missing-capability"},
	}}
	facts, err := runtimePreflightStageRunnerFacts(
		cfg,
		workflow.Definition{Name: "implement", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{Name: "review"}}}},
		requirements,
		cfg.PlacementInventory(runnersolve.HostOS()),
	)
	if err != nil {
		t.Fatalf("runtimePreflightStageRunnerFacts: %v", err)
	}
	if facts["review"].Outcome != "unsupported" {
		t.Fatalf("control-plane unsat runner fact = %+v, want unsupported", facts["review"])
	}
}

func TestRuntimePreflightRunnerSpecsCarrySolverSelfOS(t *testing.T) {
	cfg := &instance.Config{Runners: []instance.RunnerEntry{{
		Name: "local",
		Host: instance.RunnerHostSelfName,
	}}}
	inventory := cfg.PlacementInventory("windows")
	specs, err := runtimePreflightRunnerSpecs(cfg, inventory)
	if err != nil {
		t.Fatalf("runtimePreflightRunnerSpecs: %v", err)
	}
	if got := specs["local"].OS; got != "windows" {
		t.Fatalf("self runner OS = %q, want solver-substituted windows", got)
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
		map[string]runtimePreflightStageRunner{"review": runtimePreflightRunnerUnobservable("unit test")},
	)
	if len(stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(stages))
	}
	if !containsString(stages[0].RequiredCapabilities, "node@20") || !containsString(stages[0].RequiredCapabilities, "harness:copilot") {
		t.Fatalf("gate required capabilities = %v, want effective placement requirements", stages[0].RequiredCapabilities)
	}
}

func assertSelectedRunner(t *testing.T, stage runtimePreflightStage, name, kind string) {
	t.Helper()
	if stage.Runner.Outcome != "selected" || stage.Runner.Selected == nil {
		t.Fatalf("%s runner = %+v, want selected runner", stage.Name, stage.Runner)
	}
	if stage.Runner.Selected.Name != name || stage.Runner.Selected.Kind != kind {
		t.Fatalf("%s selected runner = %+v, want %s/%s", stage.Name, *stage.Runner.Selected, name, kind)
	}
}

func normalizedRuntimePreflightJSON(t *testing.T, report runtimePreflightReport) []byte {
	t.Helper()
	report.Instance.Path = "<instance-root>"
	report.Instance.ConfigDir = "<instance-config>"
	report.Workflow.Digest = "sha256:<workflow>"
	report.Workflow.GooberDigest = "sha256:<goober>"
	for si := range report.Stages {
		normalizeRunner := func(runner *runtimePreflightRunner) {
			if runner != nil && runner.Kind == "self" && runner.OS != "" {
				runner.OS = "<host-os>"
			}
		}
		normalizeRunner(report.Stages[si].Runner.Selected)
		for ri := range report.Stages[si].Runner.Eligible {
			normalizeRunner(&report.Stages[si].Runner.Eligible[ri])
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal normalized report: %v", err)
	}
	return append(data, '\n')
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
engine:
  hostPort: 127.0.0.1:7233
  namespace: default
  taskQueue: goobers-engine
runners:
  - name: local
    host: self
    provides:
      os: macOS
      capabilities:
        - node@20
  - name: linux-pool
    host: ghcr.io/example/goobers-runner:latest
    provides:
      os: linux
      capabilities:
        - node@20
      harnesses:
        - copilot
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
