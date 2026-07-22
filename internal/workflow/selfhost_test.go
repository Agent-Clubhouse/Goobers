package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestSelfhostWorkflowsCompile is #124's divergence guard: it compiles the
// REAL selfhost/ definitions (this repo's own dogfood config) directly,
// against the compiler's full admission checks (capabilities, harness, and
// gate-outcome coverage). testdata/shipped/*.yaml are separately maintained,
// deliberately minimal synthetic fixtures pinned to golden digests — nothing
// previously compiled the actual selfhost YAML, so it could (and did, per
// #124's architect review of testdata/shipped/implementation.yaml) drift
// invalid without any test catching it.
func TestSelfhostWorkflowsCompile(t *testing.T) {
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author"} {
		var g apiv1.Goober
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}
			if _, err := compileAcknowledged(def, WithGoobers(goobers)); err != nil {
				t.Fatalf("compile %s against selfhost's real goobers: %v", file, err)
			}
			if file == "backlog-curation.yaml" {
				if warnings := CheckWarnings(def); len(warnings) != 0 {
					t.Fatalf("%s warnings = %v, want warning-clean reference config", file, warnings)
				}
			}
		})
	}
}

func TestSelfhostTelemetryQueriesDeclareResultFile(t *testing.T) {
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows")
	for _, file := range []string{"work-nomination.yaml", "tutor.yaml"} {
		t.Run(file, func(t *testing.T) {
			wantResultFile := "telemetry-signals.json"
			if file == "work-nomination.yaml" {
				wantResultFile = "candidate-findings.json"
			}
			raw, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			for _, task := range w.Spec.Tasks {
				if task.Name == "gather-signals" {
					if got := task.Inputs["resultFile"]; got != wantResultFile {
						t.Fatalf("gather-signals resultFile = %q, want %s", got, wantResultFile)
					}
					return
				}
			}
			t.Fatal("gather-signals task not found")
		})
	}
}

func TestSelfhostImplementationCIPollDeclaresRequiredCapability(t *testing.T) {
	path := filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows", "implementation.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read implementation workflow: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal implementation workflow: %v", err)
	}
	for _, task := range w.Spec.Tasks {
		if task.Inputs["kind"] != "ci-poll" {
			continue
		}
		for _, declared := range task.Capabilities {
			if declared == "github:pr:write" {
				return
			}
		}
		t.Fatalf("ci-poll task %q capabilities = %v, want github:pr:write", task.Name, task.Capabilities)
	}
	t.Fatal("implementation workflow has no inputs.kind=ci-poll task")
}

// TestSelfhostAgentModelDeclarations guards model-token admission for every
// shipped agentic task. The reviewer is an agentic gate with no stage-level
// capabilities field, so its grant remains sourced from reviewer/goober.yaml.
func TestSelfhostAgentModelDeclarations(t *testing.T) {
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	taskCaps := func(t *testing.T, file, task string) []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var w apiv1.Workflow
		if err := yaml.Unmarshal(raw, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", file, err)
		}
		for _, ta := range w.Spec.Tasks {
			if ta.Name == task {
				return ta.Capabilities
			}
		}
		t.Fatalf("%s: task %q not found", file, task)
		return nil
	}
	gooberCaps := func(t *testing.T, name string) []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		var g apiv1.Goober
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		return g.Spec.Capabilities
	}
	has := func(caps []string, want string) bool {
		for _, c := range caps {
			if c == want {
				return true
			}
		}
		return false
	}

	// Each agentic task declares agent:model alongside its existing grants.
	for _, tc := range []struct {
		file, task string
		alsoNeeds  string // a pre-existing capability the addition must not drop
	}{
		{"backlog-curation.yaml", "curate", "github:issues:write"},
		{"implementation.yaml", "implement", "repo:push"},
		{"work-nomination.yaml", "nominate", "github:issues:write"},
		{"tutor.yaml", "analyze", "journal:read"},
		{"tutor.yaml", "draft-change", "repo:push"},
	} {
		caps := taskCaps(t, tc.file, tc.task)
		if !has(caps, "agent:model") {
			t.Errorf("%s/%s: expected agent:model in %v", tc.file, tc.task, caps)
		}
		if !has(caps, tc.alsoNeeds) {
			t.Errorf("%s/%s: agent:model must not drop %q (got %v)", tc.file, tc.task, tc.alsoNeeds, caps)
		}
	}

	for _, tc := range []struct {
		name, alsoNeeds string
	}{
		{"analyst", "journal:read"},
		{"config-author", "repo:push"},
	} {
		caps := gooberCaps(t, tc.name)
		if !has(caps, "agent:model") {
			t.Errorf("%s goober: expected agent:model in %v", tc.name, caps)
		}
		if !has(caps, tc.alsoNeeds) {
			t.Errorf("%s goober: agent:model must not drop %q (got %v)", tc.name, tc.alsoNeeds, caps)
		}
	}
}

func TestSelfhostTutorValidatesBeforePush(t *testing.T) {
	path := filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows", "tutor.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(raw, &tutor); err != nil {
		t.Fatal(err)
	}

	tasks := make(map[string]apiv1.Task, len(tutor.Spec.Tasks))
	for _, task := range tutor.Spec.Tasks {
		tasks[task.Name] = task
	}
	if got := tasks["draft-change"].Next; got != "validate-config" {
		t.Fatalf("draft-change next = %q, want validate-config", got)
	}
	validateTask, ok := tasks["validate-config"]
	if !ok {
		t.Fatal("tutor workflow has no validate-config task")
	}
	if validateTask.Type != apiv1.TaskDeterministic {
		t.Fatalf("validate-config type = %q, want deterministic", validateTask.Type)
	}
	if validateTask.Run == nil ||
		len(validateTask.Run.Command) != 4 ||
		validateTask.Run.Command[0] != "goobers" ||
		validateTask.Run.Command[1] != "validate" ||
		validateTask.Run.Command[2] != "--source-tree" ||
		validateTask.Run.Command[3] != "selfhost" {
		t.Fatalf("validate-config run = %+v, want direct selfhost source-tree validation", validateTask.Run)
	}
	if validateTask.Next != "config-valid" {
		t.Fatalf("validate-config next = %q, want config-valid", validateTask.Next)
	}

	for _, gate := range tutor.Spec.Gates {
		if gate.Name != "config-valid" {
			continue
		}
		if gate.Evaluator != apiv1.EvaluatorAutomated || gate.Automated == nil || gate.Automated.Check != "status-equals" {
			t.Fatalf("config-valid evaluator = %+v, want automated status-equals", gate)
		}
		if gate.Branches["pass"] != "push-branch" || gate.Branches["fail"] != "@abort" {
			t.Fatalf("config-valid branches = %v, want pass->push-branch and fail->@abort", gate.Branches)
		}
		return
	}
	t.Fatal("tutor workflow has no config-valid gate")
}
