// Package ir defines the versioned, canonical intermediate representation of
// a workflow definition. It is intentionally separate from compilation: the
// current DSL compiler remains the runtime source of truth.
package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	// SchemaVersion identifies the canonical Workflow IR schema.
	SchemaVersion = "goobers.dev/workflow-ir/v1"
	// CompilerName identifies the compiler that produced a Workflow IR document.
	CompilerName = "goobers"
	// CompilerVersion identifies the compiler contract used to produce Workflow IR.
	CompilerVersion = "workflow-ir/v1"
)

// Document is the canonical, versioned representation of a workflow definition.
type Document struct {
	SchemaVersion    string               `json:"schemaVersion"`
	Compiler         Compiler             `json:"compiler"`
	Source           Source               `json:"source"`
	Triggers         []apiv1.Trigger      `json:"triggers"`
	Start            string               `json:"start"`
	Schemas          []Schema             `json:"schemas,omitempty"`
	Nodes            []Node               `json:"nodes"`
	Edges            []Edge               `json:"edges"`
	Permissions      []string             `json:"permissions,omitempty"`
	SourceDefinition *workflow.Definition `json:"sourceDefinition,omitempty"`
	FeatureGates     []string             `json:"featureGates,omitempty"`
	Provenance       *Provenance          `json:"provenance,omitempty"`
}

// Provenance records who or what created a workflow IR document, what source
// material it came from, and how validation concluded.
type Provenance struct {
	Generator   string `json:"generator,omitempty"`
	Model       string `json:"model,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Version     string `json:"version,omitempty"`
	UserIntent  string `json:"userIntent,omitempty"`
	Source      string `json:"source,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Validation  string `json:"validation,omitempty"`
	ValidatedBy string `json:"validatedBy,omitempty"`
	Decision    string `json:"decision,omitempty"`
}

// GenerationProvenance is the persisted, source-aware record of how a workflow
// IR document was created and validated.
type GenerationProvenance = Provenance

// DiffKind classifies a semantic comparison between two IR documents.
type DiffKind string

const (
	DiffNoChange   DiffKind = "no-change"
	DiffCosmetic   DiffKind = "cosmetic"
	DiffBehavioral DiffKind = "behavioral"
)

// Change captures the smallest material difference in a semantic diff.
type Change struct {
	Path        string   `json:"path"`
	Before      string   `json:"before,omitempty"`
	After       string   `json:"after,omitempty"`
	Kind        DiffKind `json:"kind"`
	Explanation string   `json:"explanation,omitempty"`
}

// Diff summarizes whether a pair of IR documents remains equivalent or changed in
// behavior.
type Diff struct {
	Kind    DiffKind `json:"kind"`
	Summary string   `json:"summary,omitempty"`
	Changes []Change `json:"changes,omitempty"`
}

// Inspection summarizes a normalized IR document for authoring and tooling
// clients without re-reading the source definition.
type Inspection struct {
	Start           string   `json:"start"`
	TriggerTypes    []string `json:"triggerTypes,omitempty"`
	Nodes           []string `json:"nodes,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	NodeCount       int      `json:"nodeCount"`
	EdgeCount       int      `json:"edgeCount"`
	PermissionCount int      `json:"permissionCount"`
	SourceDigest    string   `json:"sourceDigest,omitempty"`
}

// Loss records where a round-trip from source->IR->source cannot retain an exact
// source-level representation.
type Loss struct {
	Field       string `json:"field,omitempty"`
	Before      string `json:"before,omitempty"`
	After       string `json:"after,omitempty"`
	Explanation string `json:"explanation"`
}

// Compiler identifies the implementation and contract version that produced a document.
type Compiler struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Source identifies the workflow definition from which a document was normalized.
type Source struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	DSLVersion string `json:"dslVersion,omitempty"`
	Digest     string `json:"digest"`
}

// Schema describes a named set of typed fields referenced by a workflow node.
type Schema struct {
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
}

// Node represents a task, gate, or parallel construct in the workflow graph.
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

// Resources describes the execution resources requested by a node.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// Parallelism describes the bounded concurrency of a parallel workflow node.
type Parallelism struct {
	Branches          int32 `json:"branches"`
	MaxConcurrent     int32 `json:"maxConcurrent,omitempty"`
	BranchTimeoutSecs int32 `json:"branchTimeoutSeconds,omitempty"`
}

// Port describes a named, typed node input or output.
type Port struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Edge represents a directed, optionally conditional workflow transition.
type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// Normalize converts a workflow definition into deterministic canonical IR.
func Normalize(def workflow.Definition) (Document, error) {
	return NormalizeWithMetadata(def, nil, nil)
}

// NormalizeWithMetadata converts a workflow definition into canonical IR and
// persists the source and generation metadata alongside the normalized graph.
func NormalizeWithMetadata(def workflow.Definition, provenance *Provenance, featureGates []string) (Document, error) {
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		return Document{}, fmt.Errorf("digest workflow definition: %w", err)
	}
	source, err := cloneDefinition(def)
	if err != nil {
		return Document{}, fmt.Errorf("snapshot workflow definition: %w", err)
	}
	doc := Document{
		SchemaVersion:    SchemaVersion,
		Compiler:         Compiler{Name: CompilerName, Version: CompilerVersion},
		Source:           Source{Name: def.Name, Version: def.Version, DSLVersion: def.DSLVersion, Digest: digest},
		Triggers:         cloneTriggers(def.Spec.Triggers),
		Start:            def.Spec.Start,
		Schemas:          []Schema{},
		Nodes:            []Node{},
		Edges:            []Edge{},
		SourceDefinition: &source,
		FeatureGates:     append([]string(nil), featureGates...),
	}
	if provenance != nil {
		copy := *provenance
		doc.Provenance = &copy
	}
	for _, task := range def.Spec.Tasks {
		node := Node{Name: task.Name, Kind: string(task.Type), Task: cloneTask(task), SideEffect: sideEffect(task)}
		node.Inputs = taskInputs(task)
		node.Outputs = ports(task.ExpectedOutputs)
		node.Timeout = task.TimeoutSeconds
		node.Retry = task.Retry.DeepCopy()
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
			doc.Edges = append(doc.Edges, Edge{From: gate.Name, To: target, Condition: condition})
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
	doc.Permissions = unique(doc.Permissions)
	sort.Strings(doc.Permissions)
	if err := Validate(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func cloneDefinition(def workflow.Definition) (workflow.Definition, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return workflow.Definition{}, err
	}
	var copy workflow.Definition
	if err := json.Unmarshal(raw, &copy); err != nil {
		return workflow.Definition{}, err
	}
	return copy, nil
}

// Digest validates the document and returns its canonical SHA-256 digest.
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

// Capabilities returns the effective permission set declared by the normalized
// document, ordered canonically for authoring and policy checks.
func (d Document) Capabilities() []string {
	seen := make(map[string]struct{}, len(d.Permissions))
	out := make([]string, 0, len(d.Permissions))
	for _, cap := range d.Permissions {
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}
	for _, node := range d.Nodes {
		if node.Task == nil {
			continue
		}
		for _, cap := range node.Task.Capabilities {
			if cap == "" {
				continue
			}
			if _, ok := seen[cap]; ok {
				continue
			}
			seen[cap] = struct{}{}
			out = append(out, cap)
		}
	}
	sort.Strings(out)
	return out
}

// Inspect returns a compact, deterministic summary of the normalized IR graph.
func (d Document) Inspect() Inspection {
	triggerTypes := make([]string, 0, len(d.Triggers))
	seenTriggers := make(map[string]struct{}, len(d.Triggers))
	for _, trigger := range d.Triggers {
		if _, ok := seenTriggers[string(trigger.Type)]; ok {
			continue
		}
		seenTriggers[string(trigger.Type)] = struct{}{}
		triggerTypes = append(triggerTypes, string(trigger.Type))
	}
	sort.Strings(triggerTypes)
	nodes := make([]string, 0, len(d.Nodes))
	for _, node := range d.Nodes {
		nodes = append(nodes, node.Name)
	}
	sort.Strings(nodes)
	caps := d.Capabilities()
	return Inspection{
		Start:           d.Start,
		TriggerTypes:    triggerTypes,
		Nodes:           nodes,
		Capabilities:    caps,
		NodeCount:       len(d.Nodes),
		EdgeCount:       len(d.Edges),
		PermissionCount: len(caps),
		SourceDigest:    d.Source.Digest,
	}
}

// SemanticDiff compares two IR documents and classifies whether any difference
// is merely cosmetic or changes workflow behavior.
func SemanticDiff(before, after Document) (Diff, error) {
	if err := Validate(before); err != nil {
		return Diff{}, fmt.Errorf("validate before document: %w", err)
	}
	if err := Validate(after); err != nil {
		return Diff{}, fmt.Errorf("validate after document: %w", err)
	}
	return before.Diff(after)
}

// Diff compares the receiver to another normalized document and classifies the
// change kind.
func formatProvenance(p *Provenance) string {
	if p == nil {
		return "<nil>"
	}
	return p.Generator + ":" + p.Model + ":" + p.Tool + ":" + p.Validation
}

func summarizeSourceDefinition(def *workflow.Definition) string {
	if def == nil {
		return "<nil>"
	}
	return def.Name + "@" + fmt.Sprintf("v%d", def.Version) + ":" + def.DSLVersion
}

func (d Document) Diff(other Document) (Diff, error) {
	if err := Validate(d); err != nil {
		return Diff{}, fmt.Errorf("validate document: %w", err)
	}
	if err := Validate(other); err != nil {
		return Diff{}, fmt.Errorf("validate other document: %w", err)
	}
	changes := make([]Change, 0, 12)
	behaviorChanged := semanticContentChanged(d, other)
	changes = appendDiffMetadataChanges(changes, d, other, behaviorChanged)
	changes = appendDiffCollectionChanges(changes, d, other)
	changes = appendDiffNodeChanges(changes, d.Nodes, other.Nodes)
	changes = appendDiffEdgeChanges(changes, d.Edges, other.Edges)
	return finalizeDiff(changes, behaviorChanged), nil
}

func appendDiffMetadataChanges(changes []Change, before, after Document, behaviorChanged bool) []Change {
	if before.Source.Digest != after.Source.Digest && behaviorChanged {
		changes = append(changes, Change{Path: "source.digest", Before: before.Source.Digest, After: after.Source.Digest, Kind: DiffBehavioral, Explanation: "source normalization digest changed with normalized workflow content"})
	}
	if before.Start != after.Start {
		changes = append(changes, Change{Path: "start", Before: before.Start, After: after.Start, Kind: DiffBehavioral, Explanation: "workflow entry point changed"})
	}
	if before.Source.DSLVersion != after.Source.DSLVersion {
		changes = append(changes, Change{Path: "source.dslVersion", Before: before.Source.DSLVersion, After: after.Source.DSLVersion, Kind: DiffCosmetic, Explanation: "source DSL version annotation changed without altering normalized behavior"})
	}
	if !reflect.DeepEqual(before.FeatureGates, after.FeatureGates) {
		changes = append(changes, Change{Path: "featureGates", Before: fmt.Sprintf("%v", before.FeatureGates), After: fmt.Sprintf("%v", after.FeatureGates), Kind: DiffCosmetic, Explanation: "feature-gate metadata changed without altering normalized workflow semantics"})
	}
	if !reflect.DeepEqual(before.SourceDefinition, after.SourceDefinition) {
		changes = append(changes, Change{Path: "sourceDefinition", Before: summarizeSourceDefinition(before.SourceDefinition), After: summarizeSourceDefinition(after.SourceDefinition), Kind: DiffCosmetic, Explanation: "persisted source metadata changed without altering normalized workflow semantics"})
	}
	if provenanceChanged(before.Provenance, after.Provenance) {
		changes = append(changes, Change{Path: "provenance", Before: formatProvenance(before.Provenance), After: formatProvenance(after.Provenance), Kind: DiffCosmetic, Explanation: "generation provenance changed without altering normalized workflow semantics"})
	}
	return changes
}

func provenanceChanged(before, after *Provenance) bool {
	if (before == nil) != (after == nil) {
		return true
	}
	if before == nil {
		return false
	}
	return *before != *after
}

func appendDiffCollectionChanges(changes []Change, before, after Document) []Change {
	if len(before.Nodes) != len(after.Nodes) {
		changes = append(changes, Change{Path: "nodes", Before: fmt.Sprintf("%d", len(before.Nodes)), After: fmt.Sprintf("%d", len(after.Nodes)), Kind: DiffBehavioral, Explanation: "node count changed"})
	}
	if len(before.Edges) != len(after.Edges) {
		changes = append(changes, Change{Path: "edges", Before: fmt.Sprintf("%d", len(before.Edges)), After: fmt.Sprintf("%d", len(after.Edges)), Kind: DiffBehavioral, Explanation: "edge count changed"})
	}
	return changes
}

func appendDiffNodeChanges(changes []Change, before, after []Node) []Change {
	for i, node := range before {
		if i >= len(after) {
			break
		}
		otherNode := after[i]
		changes = appendDiffNodeChange(changes, node, otherNode)
	}
	return changes
}

func appendDiffNodeChange(changes []Change, before, after Node) []Change {
	if before.Name != after.Name || before.Kind != after.Kind || before.SideEffect != after.SideEffect {
		changes = append(changes, Change{Path: "nodes[" + before.Name + "]", Before: before.Name + "/" + before.Kind + "/" + before.SideEffect, After: after.Name + "/" + after.Kind + "/" + after.SideEffect, Kind: DiffBehavioral, Explanation: "node identity or execution class changed"})
	}
	if before.Task != nil && after.Task != nil && before.Task.Goal != after.Task.Goal {
		changes = append(changes, Change{Path: "nodes[" + before.Name + "].task.goal", Before: before.Task.Goal, After: after.Task.Goal, Kind: DiffBehavioral, Explanation: "task intent changed"})
	}
	if before.Gate != nil && after.Gate != nil && before.Gate.Evaluator != after.Gate.Evaluator {
		changes = append(changes, Change{Path: "nodes[" + before.Name + "].gate.evaluator", Before: string(before.Gate.Evaluator), After: string(after.Gate.Evaluator), Kind: DiffBehavioral, Explanation: "gate evaluator changed"})
	}
	return changes
}

func appendDiffEdgeChanges(changes []Change, before, after []Edge) []Change {
	for i, edge := range before {
		if i >= len(after) {
			break
		}
		otherEdge := after[i]
		if edge.From != otherEdge.From || edge.To != otherEdge.To || edge.Condition != otherEdge.Condition {
			changes = append(changes, Change{Path: "edges[" + fmt.Sprintf("%d", i) + "]", Before: edge.From + "->" + edge.To + "[" + edge.Condition + "]", After: otherEdge.From + "->" + otherEdge.To + "[" + otherEdge.Condition + "]", Kind: DiffBehavioral, Explanation: "transition behavior changed"})
		}
	}
	return changes
}

func finalizeDiff(changes []Change, behaviorChanged bool) Diff {
	if behaviorChanged && !hasBehavioralChange(changes) {
		changes = append(changes, Change{
			Path:        "semantic",
			Kind:        DiffBehavioral,
			Explanation: "normalized workflow content changed outside cosmetic metadata",
		})
	}
	if len(changes) == 0 {
		return Diff{Kind: DiffNoChange, Summary: "normalized workflow IR is behaviorally equivalent"}
	}
	if hasBehavioralChange(changes) {
		return Diff{Kind: DiffBehavioral, Summary: "normalized workflow IR differs in behavior", Changes: changes}
	}
	return Diff{Kind: DiffCosmetic, Summary: "normalized workflow IR differs only in cosmetic metadata", Changes: changes}
}

func hasBehavioralChange(changes []Change) bool {
	for _, change := range changes {
		if change.Kind == DiffBehavioral {
			return true
		}
	}
	return false
}

func semanticContentChanged(before, after Document) bool {
	type semanticDocument struct {
		Triggers    []apiv1.Trigger `json:"triggers"`
		Start       string          `json:"start"`
		Schemas     []Schema        `json:"schemas"`
		Nodes       []Node          `json:"nodes"`
		Edges       []Edge          `json:"edges"`
		Permissions []string        `json:"permissions"`
	}
	left, _ := json.Marshal(semanticDocument{
		Triggers: before.Triggers, Start: before.Start, Schemas: before.Schemas,
		Nodes: before.Nodes, Edges: before.Edges, Permissions: before.Permissions,
	})
	right, _ := json.Marshal(semanticDocument{
		Triggers: after.Triggers, Start: after.Start, Schemas: after.Schemas,
		Nodes: after.Nodes, Edges: after.Edges, Permissions: after.Permissions,
	})
	return string(left) != string(right)
}

// ExplainLoss records the unavoidable information loss when a workflow source is
// converted to canonical IR and back to a generated representation.
func (d Document) ExplainLoss(source any) []Loss {
	switch src := source.(type) {
	case workflow.Definition:
		return d.explainDefinitionLoss(src)
	case *workflow.Definition:
		if src == nil {
			return typedNilSourceLoss("*workflow.Definition")
		}
		return d.explainDefinitionLoss(*src)
	case apiv1.Workflow:
		return d.explainWorkflowLoss(src)
	case *apiv1.Workflow:
		if src == nil {
			return typedNilSourceLoss("*v1alpha1.Workflow")
		}
		return d.explainWorkflowLoss(*src)
	case nil:
		return nil
	default:
		return unsupportedSourceLoss(source)
	}
	return nil
}

// RoundTripLoss is the package-level convenience wrapper used by authoring tools.
func RoundTripLoss(source any, doc Document) []Loss {
	return doc.ExplainLoss(source)
}

// Validate verifies the structural and version invariants of a Workflow IR document.
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
	if err := validateSourceDefinition(d); err != nil {
		return err
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

func (d Document) explainDefinitionLoss(src workflow.Definition) []Loss {
	loss := make([]Loss, 0, 2)
	digest, err := workflow.ComputeDigest(src)
	if err != nil || digest != d.Source.Digest {
		loss = append(loss, Loss{Field: "source.digest", Before: digest, After: d.Source.Digest, Explanation: "the persisted source does not match the IR source digest"})
	} else if d.SourceDefinition == nil {
		loss = append(loss, Loss{Field: "source.definition", Before: "workflow definition", After: "not persisted", Explanation: "the IR predates source-definition persistence, so exact round-trip fidelity cannot be established"})
	} else if !reflect.DeepEqual(src, *d.SourceDefinition) {
		loss = append(loss, Loss{Field: "source.definition", Before: "original workflow definition", After: "persisted workflow definition", Explanation: "the persisted source differs from the supplied definition"})
	}
	return loss
}

func (d Document) explainWorkflowLoss(src apiv1.Workflow) []Loss {
	loss := make([]Loss, 0, 3)
	if src.Name != d.Source.Name {
		loss = append(loss, Loss{Field: "source.name", Before: src.Name, After: d.Source.Name, Explanation: "the supplied source does not identify the persisted IR source"})
	}
	if d.SourceDefinition == nil {
		return append(loss, Loss{Field: "source.definition", Before: src.Name, After: "not persisted", Explanation: "the IR does not retain the original workflow definition needed to establish fidelity for this workflow source"})
	}
	if d.SourceDefinition.Name != src.Name {
		loss = append(loss, Loss{Field: "source.name", Before: src.Name, After: d.SourceDefinition.Name, Explanation: "the supplied workflow name does not match the persisted source definition"})
	}
	if d.SourceDefinition.DSLVersion != src.DSLVersion {
		loss = append(loss, Loss{Field: "source.dslVersion", Before: src.DSLVersion, After: d.SourceDefinition.DSLVersion, Explanation: "the supplied workflow DSL version does not match the persisted source record"})
	}
	if !reflect.DeepEqual(src.Spec, d.SourceDefinition.Spec) {
		loss = append(loss, Loss{Field: "source.spec", Before: "supplied workflow spec", After: "persisted workflow spec", Explanation: "the supplied workflow spec differs from the persisted source definition"})
	}
	return loss
}

func unsupportedSourceLoss(source any) []Loss {
	return []Loss{{
		Field:       "source.type",
		Before:      fmt.Sprintf("%T", source),
		After:       "unsupported",
		Explanation: fmt.Sprintf("round-trip loss cannot be evaluated for unsupported source type %T", source),
	}}
}

func typedNilSourceLoss(typeName string) []Loss {
	return []Loss{{
		Field:       "source",
		Before:      typeName,
		After:       "nil",
		Explanation: fmt.Sprintf("round-trip loss cannot be evaluated from a nil %s", typeName),
	}}
}

func validateSourceDefinition(d Document) error {
	if d.SourceDefinition == nil {
		return nil
	}
	if d.SourceDefinition.Name != d.Source.Name {
		return fmt.Errorf("source metadata name %q does not match persisted source definition %q", d.Source.Name, d.SourceDefinition.Name)
	}
	if d.SourceDefinition.Version != d.Source.Version {
		return fmt.Errorf("source metadata version %d does not match persisted source definition version %d", d.Source.Version, d.SourceDefinition.Version)
	}
	if d.SourceDefinition.DSLVersion != d.Source.DSLVersion {
		return fmt.Errorf("source metadata DSL version %q does not match persisted source definition %q", d.Source.DSLVersion, d.SourceDefinition.DSLVersion)
	}
	digest, err := workflow.ComputeDigest(*d.SourceDefinition)
	if err != nil {
		return fmt.Errorf("compute persisted source definition digest: %w", err)
	}
	if digest != d.Source.Digest {
		return fmt.Errorf("source metadata digest %q does not match persisted source definition digest %q", d.Source.Digest, digest)
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

func cloneTriggers(values []apiv1.Trigger) []apiv1.Trigger {
	out := make([]apiv1.Trigger, len(values))
	for i := range values {
		values[i].DeepCopyInto(&out[i])
	}
	return out
}

func cloneTask(v apiv1.Task) *apiv1.Task             { return v.DeepCopy() }
func cloneGate(v apiv1.Gate) *apiv1.Gate             { return v.DeepCopy() }
func cloneParallel(v apiv1.Parallel) *apiv1.Parallel { return v.DeepCopy() }
