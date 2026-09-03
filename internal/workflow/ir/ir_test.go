package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func TestNormalizeIsDeterministicAndPreservesSourceDigest(t *testing.T) {
	def := workflow.Definition{
		Name: "example", Version: 3, DSLVersion: "3.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "build",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks: []apiv1.Task{
				{Name: "deploy", Type: apiv1.TaskAgentic, Goal: "deploy", Goober: "ops", Next: ""},
				{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build", Next: "deploy",
					Inputs: map[string]string{"source": "main"}},
			},
		},
	}
	first, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.Digest != second.Source.Digest {
		t.Fatalf("source digest changed: %s != %s", first.Source.Digest, second.Source.Digest)
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("IR digest changed: %s != %s", d1, d2)
	}
	if got := first.Nodes[0].Name; got != "build" {
		t.Fatalf("nodes are not canonicalized by name: first is %q", got)
	}
}

func TestValidateRejectsUnsupportedAndDanglingIR(t *testing.T) {
	doc := Document{
		SchemaVersion: SchemaVersion,
		Compiler:      Compiler{Name: CompilerName, Version: CompilerVersion},
		Source:        Source{Name: "w", Digest: "sha256:test"},
		Start:         "missing",
		Nodes:         []Node{{Name: "known", Kind: "unsupported"}},
	}
	err := Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("Validate error = %v, want actionable unsupported-kind error", err)
	}
	doc.Nodes[0].Kind = string(apiv1.TaskDeterministic)
	err = Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "start node") {
		t.Fatalf("Validate error = %v, want actionable start error", err)
	}
}

func TestGoldenIRFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if err := Validate(doc); err != nil {
		t.Fatalf("golden fixture is invalid: %v", err)
	}
}
