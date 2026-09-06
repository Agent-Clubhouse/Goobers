package main

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func TestCompiledMachinesDigestResolvedInstructions(t *testing.T) {
	configDir := t.TempDir()
	instructionsDir := filepath.Join(configDir, "gaggles", "alpha", "goobers", "coder")
	if err := os.MkdirAll(instructionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	instructionsPath := filepath.Join(instructionsDir, "instructions.md")
	if err := os.WriteFile(instructionsPath, []byte("first instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Gaggle:       "alpha",
			Instructions: "instructions.md",
			Harness:      apiv1.HarnessCopilot,
			Model:        "claude-sonnet-4.5",
			Capabilities: []string{"agent:model"},
		},
	}
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implement"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "alpha",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goal: "Implement.",
				Goober: "coder", Next: workflow.TerminalComplete,
				Capabilities: []string{"agent:model"},
			}},
		},
	}}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "implement"}
	firstInstructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	first, firstDigests, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, firstInstructions, nil, nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(instructionsPath, []byte("second instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondInstructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigests, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, secondInstructions, nil, nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigests[identity] == secondDigests[identity] {
		t.Fatalf("goober digest did not change with instruction content: %s", firstDigests[identity])
	}
	if first[identity].Digest() != second[identity].Digest() {
		t.Fatalf("workflow digest changed with instruction content: %s != %s", first[identity].Digest(), second[identity].Digest())
	}
}

func TestCompiledMachinesDigestCompleteSkillPackage(t *testing.T) {
	configDir := t.TempDir()
	instructionsDir := filepath.Join(configDir, "gaggles", "alpha", "goobers", "coder")
	if err := os.MkdirAll(instructionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(instructionsDir, "instructions.md"), []byte("instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(filepath.Dir(configDir), "skills", "testing")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("Use the references."), 0o644); err != nil {
		t.Fatal(err)
	}
	referencePath := filepath.Join(skillDir, "references", "cases.md")
	if err := os.WriteFile(referencePath, []byte("original cases"), 0o644); err != nil {
		t.Fatal(err)
	}

	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Gaggle: "alpha", Instructions: "instructions.md", Skills: []string{"testing"},
			Harness: apiv1.HarnessCopilot, Model: "claude-sonnet-4.5", Capabilities: []string{"agent:model"},
		},
	}
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implement"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "alpha", Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goal: "Implement.",
				Goober: "coder", Next: workflow.TerminalComplete,
				Capabilities: []string{"agent:model"},
			}},
		},
	}}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "implement"}
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	_, before, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, nil, nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, []byte("updated cases"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, after, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, nil, nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if before[identity] == after[identity] {
		t.Fatalf("goober digest did not change with skill support file: %s", after[identity])
	}
}

func TestLoadGooberSkillPackagesPrefersGagglePackageWithSharedFallback(t *testing.T) {
	instanceRoot := t.TempDir()
	configDir := filepath.Join(instanceRoot, "config")
	sharedDir := filepath.Join(instanceRoot, "skills", "testing")
	scopedDir := filepath.Join(configDir, "gaggles", "alpha", "skills", "testing")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "SKILL.md"), []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	goobers := map[string]apiv1.GooberSpec{
		"alpha-coder": {Gaggle: "alpha", Skills: []string{"testing"}},
		"beta-coder":  {Gaggle: "beta", Skills: []string{"testing"}},
	}

	packages, err := loadGooberSkillPackages(configDir, "alpha", goobers)
	if err != nil {
		t.Fatal(err)
	}
	if got := packages["testing"]; len(got) != 1 || got[0].Content != "shared" {
		t.Fatalf("shared fallback package = %+v", got)
	}

	if err := os.MkdirAll(scopedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopedDir, "SKILL.md"), []byte("scoped"), 0o644); err != nil {
		t.Fatal(err)
	}
	packages, err = loadGooberSkillPackages(configDir, "alpha", goobers)
	if err != nil {
		t.Fatal(err)
	}
	if got := packages["testing"]; len(got) != 1 || got[0].Content != "scoped" {
		t.Fatalf("gaggle package = %+v", got)
	}

	packages, err = loadGooberSkillPackages(configDir, "beta", goobers)
	if err != nil {
		t.Fatal(err)
	}
	if got := packages["testing"]; len(got) != 1 || got[0].Content != "shared" {
		t.Fatalf("other gaggle fallback package = %+v", got)
	}
}

func TestCompiledMachinesDigestUsesAdmittedHarnessConfig(t *testing.T) {
	configDir := t.TempDir()
	instructionsDir := filepath.Join(configDir, "gaggles", "alpha", "goobers", "coder")
	if err := os.MkdirAll(instructionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instructionsDir, "instructions.md"), []byte("instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	goobers := map[string]apiv1.GooberSpec{
		"coder": {
			Gaggle:       "alpha",
			Instructions: "instructions.md",
			Harness:      apiv1.HarnessCopilot,
			Model:        "retired-model",
			Capabilities: []string{"agent:model"},
			HarnessOptions: map[string]apiextensionsv1.JSON{
				"fallback-to-default": {Raw: []byte("true")},
			},
		},
	}
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implement"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "alpha",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goal: "Implement.",
				Goober: "coder", Next: workflow.TerminalComplete,
				Capabilities: []string{"agent:model"},
			}},
		},
	}}}
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	machines, digests, resolvedGoobers, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, nil, nil,
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "implement"}
	resolvedDigest, err := workflow.ComputeGooberDigest(machines[identity].Def, resolvedGoobers, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	declaredDigest, err := workflow.ComputeGooberDigest(machines[identity].Def, goobers, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if digests[identity] != resolvedDigest || digests[identity] == declaredDigest {
		t.Fatalf("goober digest = %s, resolved = %s, declared = %s", digests[identity], resolvedDigest, declaredDigest)
	}
}
