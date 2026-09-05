package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestNormalizePreservesCanonicalGraphSemantics(t *testing.T) {
	def := workflow.Definition{
		Name: "graph", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "prepare",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Requires: &apiv1.WorkflowRequirements{Capabilities: []string{"pr.merge", "issues.write"}},
			Tasks: []apiv1.Task{
				{Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "prepare", Next: "outcome"},
				{Name: "scan-a", Type: apiv1.TaskDeterministic, Goal: "scan a", Next: workflow.TargetJoin},
				{Name: "scan-b", Type: apiv1.TaskDeterministic, Goal: "scan b", Next: workflow.TargetJoin},
				{Name: "publish", Type: apiv1.TaskAgentic, Goal: "publish", Goober: "ops",
					Capabilities: []string{"issues.write", "pr.merge"}},
			},
			Gates: []apiv1.Gate{{
				Name: "outcome", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "land-outcome"},
				Branches:  map[string]string{"merged": "fanout", "evicted": ""},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fanout", FailurePolicy: apiv1.BranchContinueOnError, Join: "publish",
				Branches: []apiv1.Branch{{Name: "left", Start: "scan-a"}, {Name: "right", Start: "scan-b"}},
			}},
		},
	}
	doc, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	wantEdges := []Edge{
		{From: "fanout", To: "scan-a", Condition: "branch:left"},
		{From: "fanout", To: "scan-b", Condition: "branch:right"},
		{From: "fanout", To: "publish", Condition: "join"},
		{From: "outcome", To: "", Condition: "evicted"},
		{From: "outcome", To: "fanout", Condition: "merged"},
		{From: "prepare", To: "outcome"},
		{From: "scan-a", To: workflow.TargetJoin},
		{From: "scan-b", To: workflow.TargetJoin},
	}
	if !reflect.DeepEqual(doc.Edges, wantEdges) {
		t.Fatalf("edges = %#v, want %#v", doc.Edges, wantEdges)
	}
	if want := []string{"issues.write", "pr.merge"}; !reflect.DeepEqual(doc.Permissions, want) {
		t.Fatalf("permissions = %v, want canonical set %v", doc.Permissions, want)
	}
	nodes := make(map[string]Node, len(doc.Nodes))
	for _, node := range doc.Nodes {
		nodes[node.Name] = node
	}
	if nodes["prepare"].SideEffect != "none" || nodes["publish"].SideEffect != "external" {
		t.Fatalf("side effects = prepare:%q publish:%q", nodes["prepare"].SideEffect, nodes["publish"].SideEffect)
	}
}

func TestNormalizeReturnsImmutableSnapshot(t *testing.T) {
	def := workflow.Definition{
		Name: "snapshot", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "task",
			Triggers: []apiv1.Trigger{{
				Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"label": "ready"},
			}},
			Tasks: []apiv1.Task{{
				Name: "task", Type: apiv1.TaskDeterministic, Goal: "task", Next: "gate",
				Inputs: map[string]string{"input": "original"}, Capabilities: []string{"issues.write"},
				Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
			}},
			Gates: []apiv1.Gate{{
				Name: "gate", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{
					Check: "output-equals", Params: map[string]string{"key": "result"},
					Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
				},
				Branches: map[string]string{"done": workflow.TerminalComplete},
			}},
		},
	}
	doc, err := Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	before, err := doc.Digest()
	if err != nil {
		t.Fatal(err)
	}
	def.Spec.Triggers[0].Selector["label"] = "changed"
	def.Spec.Tasks[0].Inputs["input"] = "changed"
	def.Spec.Tasks[0].Capabilities[0] = "repo.push"
	def.Spec.Tasks[0].Retry.MaxAttempts = 9
	def.Spec.Gates[0].Automated.Params["key"] = "changed"
	def.Spec.Gates[0].Automated.Retry.MaxAttempts = 9
	def.Spec.Gates[0].Branches["done"] = "task"
	after, err := doc.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("IR digest changed after source mutation: %s != %s", before, after)
	}
	if doc.Triggers[0].Selector["label"] != "ready" ||
		doc.Nodes[1].Task.Inputs["input"] != "original" ||
		doc.Nodes[1].Retry.MaxAttempts != 2 ||
		doc.Nodes[0].Gate.Automated.Params["key"] != "result" ||
		doc.Nodes[0].Gate.Branches["done"] != workflow.TerminalComplete {
		t.Fatalf("normalized document retained aliases into its source: %#v", doc)
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
