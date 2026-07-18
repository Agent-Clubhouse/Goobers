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
			if _, err := Compile(def, WithGoobers(goobers)); err != nil {
				t.Fatalf("compile %s against selfhost's real goobers: %v", file, err)
			}
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

// TestSelfhostAgentModelDeclarations is #289's regression guard: the live-loop
// agentic TASK stages must declare agent:model (so the Copilot subprocess is
// sourced the model token, §3.3), the curator additionally keeping
// github:issues:write; and the Tutor workflow's stages must NOT declare it
// (AC3 — tutor is out of the #30 live loop and its analyst/config-author
// goobers were never granted agent:model, so declaring it there would fail
// admission anyway). The reviewer is an agentic GATE with no stage-level
// capabilities field; its model auth is handled separately (a runner change),
// not by a stage declaration, so it's intentionally not asserted here.
func TestSelfhostAgentModelDeclarations(t *testing.T) {
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "workflows")

	taskCaps := func(t *testing.T, file, task string) []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, file))
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
	has := func(caps []string, want string) bool {
		for _, c := range caps {
			if c == want {
				return true
			}
		}
		return false
	}

	// Each live-loop agentic task declares agent:model, alongside its existing grants.
	for _, tc := range []struct {
		file, task string
		alsoNeeds  string // a pre-existing capability the addition must not drop
	}{
		{"backlog-curation.yaml", "curate", "github:issues:write"},
		{"implementation.yaml", "implement", "repo:push"},
		{"work-nomination.yaml", "nominate", "github:issues:write"},
	} {
		caps := taskCaps(t, tc.file, tc.task)
		if !has(caps, "agent:model") {
			t.Errorf("%s/%s: expected agent:model in %v", tc.file, tc.task, caps)
		}
		if !has(caps, tc.alsoNeeds) {
			t.Errorf("%s/%s: agent:model must not drop %q (got %v)", tc.file, tc.task, tc.alsoNeeds, caps)
		}
	}

	// AC3: no stage of the Tutor workflow declares agent:model.
	rawTutor, err := os.ReadFile(filepath.Join(root, "tutor.yaml"))
	if err != nil {
		t.Fatalf("read tutor.yaml: %v", err)
	}
	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(rawTutor, &tutor); err != nil {
		t.Fatalf("unmarshal tutor.yaml: %v", err)
	}
	for _, ta := range tutor.Spec.Tasks {
		if has(ta.Capabilities, "agent:model") {
			t.Errorf("tutor.yaml/%s must not declare agent:model (AC3): %v", ta.Name, ta.Capabilities)
		}
	}
}
