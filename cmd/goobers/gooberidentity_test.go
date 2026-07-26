package main

import (
	"os"
	"path/filepath"
	"testing"

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
			}},
		},
	}}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "implement"}
	firstInstructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	first, firstDigests, err := compiledMachinesWithGooberDigests(configDir, set, goobers, firstInstructions)
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
	second, secondDigests, err := compiledMachinesWithGooberDigests(configDir, set, goobers, secondInstructions)
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
			Harness: apiv1.HarnessCopilot, Model: "claude-sonnet-4.5",
		},
	}
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implement"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "alpha", Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goal: "Implement.",
				Goober: "coder", Next: workflow.TerminalComplete,
			}},
		},
	}}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "implement"}
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := compiledMachinesWithGooberDigests(configDir, set, goobers, instructions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, []byte("updated cases"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, after, err := compiledMachinesWithGooberDigests(configDir, set, goobers, instructions)
	if err != nil {
		t.Fatal(err)
	}
	if before[identity] == after[identity] {
		t.Fatalf("goober digest did not change with skill support file: %s", after[identity])
	}
}
