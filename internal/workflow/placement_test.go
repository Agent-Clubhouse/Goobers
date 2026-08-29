package workflow

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// TestStagePlacementsV30 verifies the 3.0 arm builds the full effective
// requirement: declared runsOn, derived tags (harness:<name> for agentic
// stages from the goober's harness, shell for sh/make stages), and the
// gaggle-level floor unioned in (dsl-3.0.md §2) — and, after the task rows,
// one row per agentic gate that declares runsOn (decision 001), deriving the
// REVIEWER goober's harness; an agentic gate without runsOn and an automated
// gate emit no row.
func TestStagePlacementsV30(t *testing.T) {
	def := Definition{
		Name:       "wf",
		Version:    1,
		DSLVersion: supportmatrix.V3DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Tasks: []apiv1.Task{
				{
					Name:   "implement",
					Type:   apiv1.TaskAgentic,
					Goober: "dev",
					RunsOn: &apiv1.RunsOn{
						OS:           "windows",
						CPU:          "2000m",
						Memory:       "4Gi",
						Capabilities: []string{"dotnet@8"},
						Restrictions: []string{"network:allowlist"},
					},
				},
				{
					Name: "local-ci",
					Type: apiv1.TaskDeterministic,
					Run:  &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
				},
			},
			Gates: []apiv1.Gate{
				{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
					RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Restrictions: []string{"network:allowlist"}},
					Branches: map[string]string{"pass": "", "fail": "@abort"},
				},
				{
					Name: "unplaced", Evaluator: apiv1.EvaluatorAgentic,
					Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{"pass": "", "fail": "@abort"},
				},
				{
					Name: "ci", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "ci-status"},
					Branches:  map[string]string{"pass": "", "fail": "@abort"},
				},
			},
		},
	}
	gaggle := apiv1.GaggleSpec{RunsOn: &apiv1.GaggleRunsOn{Capabilities: []string{"go@1.26"}}}
	goobers := map[string]apiv1.GooberSpec{
		"dev":      {Harness: apiv1.HarnessClaudeCode},
		"reviewer": {Harness: apiv1.HarnessCopilot},
	}

	requirements, err := StagePlacements(def, gaggle, goobers)
	if err != nil {
		t.Fatal(err)
	}
	want := []runnersolve.StageRequirement{
		{
			Stage:        "implement",
			OS:           "windows",
			CPU:          "2000m",
			Memory:       "4Gi",
			Capabilities: []string{"dotnet@8", "go@1.26", "harness:claude-code"},
			Restrictions: []string{"network:allowlist"},
		},
		{
			Stage:        "local-ci",
			Capabilities: []string{"go@1.26", "run:shell"},
		},
		{
			Stage:        "review",
			CPU:          "1000m",
			Memory:       "2Gi",
			Capabilities: []string{"go@1.26", "harness:copilot"},
			Restrictions: []string{"network:allowlist"},
		},
	}
	if !reflect.DeepEqual(requirements, want) {
		t.Fatalf("requirements = %#v, want %#v", requirements, want)
	}
}

// TestStagePlacementsPreV30 verifies the pre-3.0 arm degrades to the declared
// requiredCapabilities union with the gaggle floor — no OS, no quantities, no
// restrictions, no derivation, and no gate rows — so a byte-untouched 2.0
// workflow produces the same admission input as every previous release. The
// fixture gate DECLARES runsOn so the no-gate-rows property is load-bearing:
// a pre-3.0 arm that placed gates would emit a "review" row here and fail
// (the router refuses the field separately, via preV30SurfaceProblems).
func TestStagePlacementsPreV30(t *testing.T) {
	def := Definition{
		Name:       "wf",
		Version:    1,
		DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Tasks: []apiv1.Task{
				{
					Name:                 "implement",
					Type:                 apiv1.TaskAgentic,
					Goober:               "dev",
					RequiredCapabilities: []string{"dotnet@8", "os=windows"},
				},
				{
					Name: "local-ci",
					Type: apiv1.TaskDeterministic,
					Run:  &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
				},
			},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi"},
				Branches: map[string]string{"pass": "", "fail": "@abort"},
			}},
		},
	}
	gaggle := apiv1.GaggleSpec{RequiredCapabilities: []string{"go@1.26", "dotnet@8"}}

	requirements, err := StagePlacements(def, gaggle, map[string]apiv1.GooberSpec{"dev": {Harness: apiv1.HarnessClaudeCode}})
	if err != nil {
		t.Fatal(err)
	}
	want := []runnersolve.StageRequirement{
		{Stage: "implement", Capabilities: []string{"dotnet@8", "os=windows", "go@1.26"}},
		{Stage: "local-ci", Capabilities: []string{"go@1.26", "dotnet@8"}},
	}
	if !reflect.DeepEqual(requirements, want) {
		t.Fatalf("requirements = %#v, want %#v", requirements, want)
	}
	for _, requirement := range requirements {
		if requirement.Stage == "review" {
			t.Fatalf("pre-3.0 arm must emit no gate row even for a gate declaring runsOn: %#v", requirement)
		}
		if requirement.OS != "" || requirement.CPU != "" || requirement.Memory != "" || requirement.Disk != "" || requirement.Restrictions != nil {
			t.Fatalf("pre-3.0 requirement must carry only capabilities: %#v", requirement)
		}
		for _, token := range requirement.Capabilities {
			if token == "run:shell" || token == "harness:claude-code" {
				t.Fatalf("pre-3.0 documents must derive nothing (frozen interpreters): %#v", requirement)
			}
		}
	}
}

// TestStagePlacementsPreV30RefusesWindowsAdmin: the pre-3.0 arm refuses the
// one product-interpreted token instead of matching it (#3619). Without this,
// a 2.0 task naming privilege=windows-admin in requiredCapabilities would
// reach the solver with no OS and no coherence rule, pin to whichever class
// claims the token, and render ContainerAdministrator — through validate's
// checkpoint solve and the run-start pin, both of which read StagePlacements
// directly rather than through Compile. Task and gaggle-level sets alike;
// the same document on 3.0 runsOn is the accepted spelling.
func TestStagePlacementsPreV30RefusesWindowsAdmin(t *testing.T) {
	task := apiv1.Task{
		Name: "install-service", Type: apiv1.TaskDeterministic,
		Run: &apiv1.DeterministicRun{Command: []string{"install.cmd"}},
	}
	def := func(caps []string) Definition {
		t := task
		t.RequiredCapabilities = caps
		return Definition{Name: "wf", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{t}}}
	}

	_, err := StagePlacements(def([]string{"dotnet@8", runnercap.CapabilityWindowsAdmin}), apiv1.GaggleSpec{}, nil)
	if err == nil || !strings.Contains(err.Error(), `task "install-service" declares requiredCapabilities "privilege=windows-admin"`) {
		t.Fatalf("StagePlacements(2.0 task token) error = %v, want the router refusal", err)
	}

	_, err = StagePlacements(def([]string{"dotnet@8"}), apiv1.GaggleSpec{RequiredCapabilities: []string{runnercap.CapabilityWindowsAdmin}}, nil)
	if err == nil || !strings.Contains(err.Error(), `the gaggle declares requiredCapabilities "privilege=windows-admin"`) {
		t.Fatalf("StagePlacements(2.0 gaggle token) error = %v, want the router refusal", err)
	}

	// Any other token is still an opaque tag the pre-3.0 arm passes through.
	requirements, err := StagePlacements(def([]string{"dotnet@8", "privilege=other"}), apiv1.GaggleSpec{}, nil)
	if err != nil {
		t.Fatalf("StagePlacements(2.0 plain tokens) = %v, want success", err)
	}
	if !reflect.DeepEqual(requirements, []runnersolve.StageRequirement{{Stage: "install-service", Capabilities: []string{"dotnet@8", "privilege=other"}}}) {
		t.Fatalf("requirements = %#v, want the declared tags verbatim", requirements)
	}

	// The 3.0 spelling of the same requirement is accepted and carries the OS
	// the coherence rule demands.
	v30Task := task
	v30Task.RunsOn = &apiv1.RunsOn{OS: "windows", Capabilities: []string{"dotnet@8", runnercap.CapabilityWindowsAdmin}}
	requirements, err = StagePlacements(Definition{Name: "wf", Version: 1, DSLVersion: supportmatrix.V3DSLVersion,
		Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{v30Task}}}, apiv1.GaggleSpec{}, nil)
	if err != nil {
		t.Fatalf("StagePlacements(3.0 runsOn token) = %v, want success", err)
	}
	if len(requirements) != 1 || requirements[0].OS != "windows" || !runnercap.HasWindowsAdmin(requirements[0].Capabilities) {
		t.Fatalf("requirements = %#v, want one windows row carrying the token", requirements)
	}
}
