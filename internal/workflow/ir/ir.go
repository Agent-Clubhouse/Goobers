// Package ir defines the versioned, canonical intermediate representation of
// a workflow definition. It is intentionally separate from compilation: the
// current DSL compiler remains the runtime source of truth.
package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	SchemaVersion   = "goobers.dev/workflow-ir/v1"
	CompilerName    = "goobers"
	CompilerVersion = "workflow-ir/v1"
)

type Document struct {
	SchemaVersion string          `json:"schemaVersion"`
	Compiler      Compiler        `json:"compiler"`
	Source        Source          `json:"source"`
	Triggers      []apiv1.Trigger `json:"triggers"`
	Start         string          `json:"start"`
	Schemas       []Schema        `json:"schemas,omitempty"`
	Nodes         []Node          `json:"nodes"`
	Edges         []Edge          `json:"edges"`
	Permissions   []string        `json:"permissions,omitempty"`
}

type Compiler struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Source struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	DSLVersion string `json:"dslVersion,omitempty"`
	Digest     string `json:"digest"`
}

type Schema struct {
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
}

type Node struct {
	Name        string             `json:"name"`
	Kind        string             `json:"kind"`
	Inputs      []Port             `json:"inputs,omitempty"`
	Outputs     []Port             `json:"outputs,omitempty"`
	Task        *apiv1.Task        `json:"task,omitempty"`
	Gate        *apiv1.Gate        `json:"gate,omitempty"`
	Parallel    *apiv1.Parallel    `json:"parallel,omitempty"`
	SideEffect  string             `json:"sideEffect"`
	Timeout     int32              `json:"timeoutSeconds,omitempty"`
	Retry       *apiv1.RetryPolicy `json:"retry,omitempty"`
	Resources   *Resources         `json:"resources,omitempty"`
	Parallelism *Parallelism       `json:"parallelism,omitempty"`
	HumanGate   bool               `json:"humanGate,omitempty"`
}

type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

type Parallelism struct {
	Branches          int32 `json:"branches"`
	MaxConcurrent     int32 `json:"maxConcurrent,omitempty"`
	BranchTimeoutSecs int32 `json:"branchTimeoutSeconds,omitempty"`
}

type Port struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

func Normalize(def workflow.Definition) (Document, error) {
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		return Document{}, fmt.Errorf("digest workflow definition: %w", err)
	}
	doc := Document{
		SchemaVersion: SchemaVersion,
		Compiler:      Compiler{Name: CompilerName, Version: CompilerVersion},
		Source:        Source{Name: def.Name, Version: def.Version, DSLVersion: def.DSLVersion, Digest: digest},
		Triggers:      append([]apiv1.Trigger(nil), def.Spec.Triggers...),
		Start:         def.Spec.Start,
		Schemas:       []Schema{},
		Nodes:         []Node{},
		Edges:         []Edge{},
	}
	for _, task := range def.Spec.Tasks {
		node := Node{Name: task.Name, Kind: string(task.Type), Task: cloneTask(task), SideEffect: sideEffect(task)}
		node.Inputs = taskInputs(task)
		node.Outputs = ports(task.ExpectedOutputs)
		node.Timeout = task.TimeoutSeconds
		node.Retry = task.Retry
		if task.RunsOn != nil {
			node.Resources = &Resources{CPU: task.RunsOn.CPU, Memory: task.RunsOn.Memory, Disk: task.RunsOn.Disk}
		}
		doc.Nodes = append(doc.Nodes, node)
		doc.Schemas = append(doc.Schemas, taskSchema(task))
		if task.Next != "" {
			doc.Edges = append(doc.Edges, Edge{From: task.Name, To: task.Next})
		}
	}
	for _, gate := range def.Spec.Gates {
		node := Node{Name: gate.Name, Kind: "gate", Gate: cloneGate(gate), SideEffect: "none", HumanGate: gate.Evaluator == apiv1.EvaluatorHuman}
		doc.Nodes = append(doc.Nodes, node)
		for condition, target := range gate.Branches {
			if target != "" {
				doc.Edges = append(doc.Edges, Edge{From: gate.Name, To: target, Condition: condition})
			}
		}
	}
	for _, parallel := range def.Spec.Parallels {
		node := Node{
			Name: parallel.Name, Kind: "parallel", Parallel: cloneParallel(parallel),
			SideEffect: "none",
			Parallelism: &Parallelism{
				Branches: int32(len(parallel.Branches)), MaxConcurrent: parallel.MaxConcurrentBranches,
				BranchTimeoutSecs: parallel.BranchTimeoutSeconds,
			},
		}
		doc.Nodes = append(doc.Nodes, node)
		for _, branch := range parallel.Branches {
			doc.Edges = append(doc.Edges, Edge{From: parallel.Name, To: branch.Start, Condition: "branch:" + branch.Name})
		}
		if parallel.Join != "" {
			doc.Edges = append(doc.Edges, Edge{From: parallel.Name, To: parallel.Join, Condition: "join"})
		}
		if parallel.OnFailure != "" {
			doc.Edges = append(doc.Edges, Edge{From: parallel.Name, To: parallel.OnFailure, Condition: "failure"})
		}
	}
	for _, task := range def.Spec.Tasks {
		doc.Permissions = append(doc.Permissions, task.Capabilities...)
	}
	if def.Spec.Requires != nil {
		doc.Permissions = append(doc.Permissions, def.Spec.Requires.Capabilities...)
	}
	sort.Slice(doc.Nodes, func(i, j int) bool {
		return doc.Nodes[i].Name < doc.Nodes[j].Name
	})
	sort.Slice(doc.Schemas, func(i, j int) bool {
		return doc.Schemas[i].Name < doc.Schemas[j].Name
	})
	sort.Slice(doc.Edges, func(i, j int) bool {
		if doc.Edges[i].From != doc.Edges[j].From {
			return doc.Edges[i].From < doc.Edges[j].From
		}
		if doc.Edges[i].Condition != doc.Edges[j].Condition {
			return doc.Edges[i].Condition < doc.Edges[j].Condition
		}
		return doc.Edges[i].To < doc.Edges[j].To
	})
	sort.Strings(doc.Permissions)
	if err := Validate(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func (d Document) Digest() (string, error) {
	if err := Validate(d); err != nil {
		return "", err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal workflow IR: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func Validate(d Document) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported IR schema %q (want %q)", d.SchemaVersion, SchemaVersion)
	}
	if d.Compiler.Name == "" || d.Compiler.Version == "" {
		return fmt.Errorf("compiler metadata requires name and version")
	}
	if d.Source.Name == "" || d.Source.Digest == "" {
		return fmt.Errorf("source metadata requires name and digest")
	}
	names := map[string]bool{}
	for _, n := range d.Nodes {
		if n.Name == "" {
			return fmt.Errorf("node name must not be empty")
		}
		if names[n.Name] {
			return fmt.Errorf("duplicate node %q", n.Name)
		}
		names[n.Name] = true
		switch n.Kind {
		case string(apiv1.TaskDeterministic), string(apiv1.TaskAgentic), "gate", "parallel":
		default:
			return fmt.Errorf("node %q has unsupported kind %q", n.Name, n.Kind)
		}
		if n.SideEffect != "none" && n.SideEffect != "external" {
			return fmt.Errorf("node %q has unsupported side-effect class %q", n.Name, n.SideEffect)
		}
		switch n.Kind {
		case "gate":
			if n.Gate == nil {
				return fmt.Errorf("gate node %q is missing its definition", n.Name)
			}
			if n.Task != nil || n.Parallel != nil {
				return fmt.Errorf("gate node %q has an inconsistent payload", n.Name)
			}
			if n.Gate.Name != n.Name {
				return fmt.Errorf("gate node %q has mismatched definition name %q", n.Name, n.Gate.Name)
			}
			if len(n.Gate.Branches) == 0 {
				return fmt.Errorf("gate node %q must declare branches", n.Name)
			}
			if _, ok := n.Gate.Branches["pass"]; !ok {
				return fmt.Errorf("gate node %q must declare a pass branch", n.Name)
			}
			payloads := 0
			if n.Gate.Automated != nil {
				payloads++
			}
			if n.Gate.Agentic != nil {
				payloads++
			}
			if n.Gate.Human != nil {
				payloads++
			}
			if payloads != 1 {
				return fmt.Errorf("gate node %q must declare exactly one evaluator payload", n.Name)
			}
			switch n.Gate.Evaluator {
			case apiv1.EvaluatorAutomated:
				if n.Gate.Automated == nil || n.Gate.Agentic != nil || n.Gate.Human != nil {
					return fmt.Errorf("gate node %q has an inconsistent automated evaluator", n.Name)
				}
				if n.Gate.Automated.Check == "" {
					return fmt.Errorf("gate node %q automated evaluator requires a check", n.Name)
				}
			case apiv1.EvaluatorAgentic:
				if n.Gate.Agentic == nil || n.Gate.Automated != nil || n.Gate.Human != nil {
					return fmt.Errorf("gate node %q has an inconsistent agentic evaluator", n.Name)
				}
				if n.Gate.Agentic.Goober == "" {
					return fmt.Errorf("gate node %q agentic evaluator requires a goober", n.Name)
				}
			case apiv1.EvaluatorHuman:
				if n.Gate.Human == nil || n.Gate.Automated != nil || n.Gate.Agentic != nil {
					return fmt.Errorf("gate node %q has an inconsistent human evaluator", n.Name)
				}
			default:
				return fmt.Errorf("gate node %q has unsupported evaluator %q", n.Name, n.Gate.Evaluator)
			}
		case "parallel":
			if n.Parallel == nil {
				return fmt.Errorf("parallel node %q is missing its definition", n.Name)
			}
			if n.Task != nil || n.Gate != nil {
				return fmt.Errorf("parallel node %q has an inconsistent payload", n.Name)
			}
			if n.Parallel.Name != n.Name {
				return fmt.Errorf("parallel node %q has mismatched definition name %q", n.Name, n.Parallel.Name)
			}
			if len(n.Parallel.Branches) < 2 || n.Parallel.Join == "" {
				return fmt.Errorf("parallel node %q requires at least two branches and a join", n.Name)
			}
			if n.Parallel.FailurePolicy == apiv1.BranchContinueOnError && n.Parallel.OnFailure != "" {
				return fmt.Errorf("parallel node %q cannot set onFailure with continue_on_error", n.Name)
			}
			if n.Parallel.FailurePolicy != apiv1.BranchContinueOnError && n.Parallel.OnFailure == "" {
				return fmt.Errorf("parallel node %q requires onFailure for its failure policy", n.Name)
			}
			if n.Parallel.BranchTimeoutSeconds < 0 || n.Parallel.MaxConcurrentBranches < 0 {
				return fmt.Errorf("parallel node %q has invalid parallelism limits", n.Name)
			}
		default:
			if n.Task == nil {
				return fmt.Errorf("task node %q is missing its definition", n.Name)
			}
			if n.Gate != nil || n.Parallel != nil {
				return fmt.Errorf("task node %q has an inconsistent payload", n.Name)
			}
			if n.Task.Name != n.Name || n.Task.Type != apiv1.TaskType(n.Kind) {
				return fmt.Errorf("task node %q has a mismatched definition", n.Name)
			}
		}
		if err := validatePorts(n.Name, n.Inputs); err != nil {
			return err
		}
		if err := validatePorts(n.Name, n.Outputs); err != nil {
			return err
		}
	}
	if err := validateSchemas(d.Schemas); err != nil {
		return err
	}
	if d.Start == "" || !names[d.Start] {
		return fmt.Errorf("start node %q is not declared", d.Start)
	}
	for _, edge := range d.Edges {
		if !names[edge.From] {
			return fmt.Errorf("edge source %q is not declared", edge.From)
		}
		if edge.To != "" && edge.To[0] == '@' && !workflow.IsReservedAnyTarget(edge.To) {
			return fmt.Errorf("edge target %q from %q is unsupported", edge.To, edge.From)
		}
		if edge.To != "" && edge.To[0] != '@' && !names[edge.To] {
			return fmt.Errorf("edge target %q from %q is not declared", edge.To, edge.From)
		}
	}
	for _, trigger := range d.Triggers {
		switch trigger.Type {
		case apiv1.TriggerManual, apiv1.TriggerBacklogItem, apiv1.TriggerSchedule, apiv1.TriggerSignal, apiv1.TriggerWebhook:
		default:
			return fmt.Errorf("unsupported trigger type %q", trigger.Type)
		}
	}
	return nil
}

func ports(names []string) []Port {
	names = append([]string(nil), names...)
	sort.Strings(names)
	out := make([]Port, 0, len(names))
	for _, name := range names {
		out = append(out, Port{Name: name, Type: "string"})
	}
	return out
}

func taskInputs(task apiv1.Task) []Port {
	names := make([]string, 0, len(task.Inputs)+len(task.InputsFrom))
	for name := range task.Inputs {
		names = append(names, name)
	}
	for name := range task.InputsFrom {
		names = append(names, name)
	}
	return ports(unique(names))
}

func taskSchema(task apiv1.Task) Schema {
	fields := make(map[string]string, len(task.Inputs)+len(task.InputsFrom)+len(task.ExpectedOutputs))
	for _, port := range taskInputs(task) {
		fields[port.Name] = port.Type
	}
	for _, port := range ports(task.ExpectedOutputs) {
		fields[port.Name] = port.Type
	}
	return Schema{Name: task.Name, Fields: fields}
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func validatePorts(node string, ports []Port) error {
	seen := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		if port.Name == "" || port.Type == "" {
			return fmt.Errorf("node %q has an incomplete port", node)
		}
		if port.Type != "string" {
			return fmt.Errorf("node %q has unsupported port type %q", node, port.Type)
		}
		if _, ok := seen[port.Name]; ok {
			return fmt.Errorf("node %q has duplicate port %q", node, port.Name)
		}
		seen[port.Name] = struct{}{}
	}
	return nil
}

func validateSchemas(schemas []Schema) error {
	seen := make(map[string]struct{}, len(schemas))
	for _, schema := range schemas {
		if schema.Name == "" {
			return fmt.Errorf("schema name must not be empty")
		}
		if _, ok := seen[schema.Name]; ok {
			return fmt.Errorf("duplicate schema %q", schema.Name)
		}
		seen[schema.Name] = struct{}{}
		for name, typ := range schema.Fields {
			if name == "" || typ != "string" {
				return fmt.Errorf("schema %q has unsupported field %q of type %q", schema.Name, name, typ)
			}
		}
	}
	return nil
}

func sideEffect(t apiv1.Task) string {
	if len(t.Capabilities) == 0 && t.Type == apiv1.TaskDeterministic {
		return "none"
	}
	return "external"
}

func cloneTask(v apiv1.Task) *apiv1.Task             { return &v }
func cloneGate(v apiv1.Gate) *apiv1.Gate             { return &v }
func cloneParallel(v apiv1.Parallel) *apiv1.Parallel { return &v }
