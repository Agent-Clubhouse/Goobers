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
	doc.Nodes[0].SideEffect = "none"
	doc.Nodes[0].Task = &apiv1.Task{Name: "known", Type: apiv1.TaskDeterministic, Goal: "known"}
	err = Validate(doc)
	if err == nil || !strings.Contains(err.Error(), "start node") {
		t.Fatalf("Validate error = %v, want actionable start error", err)
	}
}

func TestNormalizeEmitsTypedPortsAndSchemas(t *testing.T) {
	def := workflow.Definition{
		Name: "ports", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "task",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks: []apiv1.Task{{
				Name: "task", Type: apiv1.TaskDeterministic, Goal: "task",
				Inputs: map[string]string{"literal": "value"}, InputsFrom: map[string]string{"upstream": "result"},
				ExpectedOutputs: []string{"result"},
			}},
		},
	}
	doc, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Schemas) != 1 || doc.Schemas[0].Fields["result"] != "string" {
		t.Fatalf("schemas = %#v, want task schema containing typed result", doc.Schemas)
	}
	if len(doc.Nodes[0].Inputs) != 2 || len(doc.Nodes[0].Outputs) != 1 || doc.Nodes[0].Outputs[0].Type != "string" {
		t.Fatalf("ports = %#v, want two inputs and one typed output", doc.Nodes[0])
	}
}

func TestValidateRejectsInconsistentEvaluatorAndParallelConfiguration(t *testing.T) {
	base := Document{
		SchemaVersion: SchemaVersion,
		Compiler:      Compiler{Name: CompilerName, Version: CompilerVersion},
		Source:        Source{Name: "w", Digest: "sha256:test"},
		Start:         "gate",
		Nodes: []Node{{Name: "gate", Kind: "gate", SideEffect: "none", Gate: &apiv1.Gate{
			Name: "gate", Evaluator: apiv1.EvaluatorHuman, Branches: map[string]string{"pass": ""},
		}}},
	}
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "exactly one evaluator") {
		t.Fatalf("Validate evaluator error = %v, want evaluator-specific error", err)
	}
	base.Nodes[0] = Node{Name: "fanout", Kind: "parallel", SideEffect: "none", Parallel: &apiv1.Parallel{
		Name: "fanout", FailurePolicy: apiv1.BranchFailFast,
		Branches: []apiv1.Branch{{Name: "only", Start: "missing"}}, Join: "missing",
	}}
	base.Start = "fanout"
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "at least two branches") {
		t.Fatalf("Validate parallel error = %v, want structural error", err)
	}
}

func TestGoldenIRFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want Document
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := Validate(want); err != nil {
		t.Fatalf("golden fixture is invalid: %v", err)
	}
	def := workflow.Definition{
		Name: "golden", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "golden", Start: "build",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Tasks:    []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build"}},
		},
	}
	got, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.Digest != want.Source.Digest {
		t.Fatalf("golden source digest = %q, want normalized digest %q", want.Source.Digest, got.Source.Digest)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("golden IR does not match normalized definition:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}
