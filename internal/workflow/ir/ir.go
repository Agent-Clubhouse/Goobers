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

// Diff compares the receiver to another normalized document and classifies the
// change kind.
func (d Document) Diff(other Document) (Diff, error) {
	if err := Validate(d); err != nil {
		return Diff{}, fmt.Errorf("validate document: %w", err)
	}
	if err := Validate(other); err != nil {
		return Diff{}, fmt.Errorf("validate other document: %w", err)
	}
	changes := collectDiffChanges(d, other)
	if len(changes) == 0 {
		return Diff{Kind: DiffNoChange, Summary: "normalized workflow IR is behaviorally equivalent"}, nil
	}
	if hasBehavioralChange(changes) {
		return Diff{Kind: DiffBehavioral, Summary: "normalized workflow IR differs in behavior", Changes: changes}, nil
	}
	return Diff{Kind: DiffCosmetic, Summary: "normalized workflow IR differs only in cosmetic metadata", Changes: changes}, nil
}

func collectDiffChanges(d, other Document) []Change {
	changes := make([]Change, 0, 16)
	appendCosmeticDiffs(&changes, d, other)
	appendBehavioralDiffs(&changes, d, other)
	return changes
}

func appendCosmeticDiffs(changes *[]Change, d, other Document) {
	appendDiffChange(changes, d.Compiler != other.Compiler, "compiler", fmt.Sprintf("%v", d.Compiler), fmt.Sprintf("%v", other.Compiler), DiffCosmetic, "compiler metadata changed without altering normalized workflow semantics")
	appendDiffChange(changes, d.Source.Name != other.Source.Name, "source.name", d.Source.Name, other.Source.Name, DiffCosmetic, "source name changed without altering normalized behavior")
	appendDiffChange(changes, d.Source.Version != other.Source.Version, "source.version", fmt.Sprintf("%d", d.Source.Version), fmt.Sprintf("%d", other.Source.Version), DiffCosmetic, "source version changed without altering normalized behavior")
	appendDiffChange(changes, d.Source.DSLVersion != other.Source.DSLVersion, "source.dslVersion", d.Source.DSLVersion, other.Source.DSLVersion, DiffCosmetic, "source DSL version annotation changed without altering normalized behavior")
	appendDiffChange(changes, d.Source.Digest != other.Source.Digest, "source.digest", d.Source.Digest, other.Source.Digest, DiffCosmetic, "source digest changed without altering normalized workflow semantics")
	appendDiffChange(changes, !reflect.DeepEqual(d.FeatureGates, other.FeatureGates), "featureGates", fmt.Sprintf("%v", d.FeatureGates), fmt.Sprintf("%v", other.FeatureGates), DiffCosmetic, "feature-gate metadata changed without altering normalized workflow semantics")
	appendDiffChange(changes, !reflect.DeepEqual(d.SourceDefinition, other.SourceDefinition), "sourceDefinition", summarizeSourceDefinition(d.SourceDefinition), summarizeSourceDefinition(other.SourceDefinition), DiffCosmetic, "persisted source metadata changed without altering normalized workflow semantics")
	if (d.Provenance == nil) != (other.Provenance == nil) || (d.Provenance != nil && other.Provenance != nil && *d.Provenance != *other.Provenance) {
		*changes = append(*changes, Change{Path: "provenance", Before: formatProvenance(d.Provenance), After: formatProvenance(other.Provenance), Kind: DiffCosmetic, Explanation: "generation provenance changed without altering normalized workflow semantics"})
	}
}

func appendBehavioralDiffs(changes *[]Change, d, other Document) {
	appendDiffChange(changes, d.Start != other.Start, "start", d.Start, other.Start, DiffBehavioral, "workflow entry point changed")
	appendDiffChange(changes, !sameTriggers(d.Triggers, other.Triggers), "triggers", fmt.Sprintf("%v", d.Triggers), fmt.Sprintf("%v", other.Triggers), DiffBehavioral, "workflow triggers changed")
	appendDiffChange(changes, !sameStringSet(d.Permissions, other.Permissions), "permissions", fmt.Sprintf("%v", d.Permissions), fmt.Sprintf("%v", other.Permissions), DiffBehavioral, "workflow permissions changed")
	appendDiffChange(changes, !sameSchemas(d.Schemas, other.Schemas), "schemas", fmt.Sprintf("%v", d.Schemas), fmt.Sprintf("%v", other.Schemas), DiffBehavioral, "workflow schemas changed")
	appendDiffChange(changes, len(d.Nodes) != len(other.Nodes), "nodes", fmt.Sprintf("%d", len(d.Nodes)), fmt.Sprintf("%d", len(other.Nodes)), DiffBehavioral, "node count changed")
	appendDiffChange(changes, len(d.Edges) != len(other.Edges), "edges", fmt.Sprintf("%d", len(d.Edges)), fmt.Sprintf("%d", len(other.Edges)), DiffBehavioral, "edge count changed")
	appendNodeDiffs(changes, d.Nodes, other.Nodes)
	appendEdgeDiffs(changes, d.Edges, other.Edges)
}

func appendDiffChange(changes *[]Change, changed bool, path, before, after string, kind DiffKind, explanation string) {
	if !changed {
		return
	}
	*changes = append(*changes, Change{Path: path, Before: before, After: after, Kind: kind, Explanation: explanation})
}

func appendNodeDiffs(changes *[]Change, before, after []Node) {
	afterMap := make(map[string]Node, len(after))
	for _, node := range after {
		afterMap[node.Name] = node
	}
	beforeMap := make(map[string]Node, len(before))
	for _, node := range before {
		beforeMap[node.Name] = node
	}
	for _, node := range before {
		otherNode, ok := afterMap[node.Name]
		if !ok {
			*changes = append(*changes, Change{Path: "nodes[" + node.Name + "]", Before: node.Name, After: "<none>", Kind: DiffBehavioral, Explanation: "node removed"})
			continue
		}
		compareSingleNode(changes, node, otherNode)
	}
	for _, otherNode := range after {
		if _, ok := beforeMap[otherNode.Name]; !ok {
			*changes = append(*changes, Change{Path: "nodes[" + otherNode.Name + "]", Before: "<none>", After: otherNode.Name, Kind: DiffBehavioral, Explanation: "node added"})
		}
	}
}

func compareSingleNode(changes *[]Change, node, otherNode Node) {
	if node.Kind != otherNode.Kind || node.SideEffect != otherNode.SideEffect {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "]", Before: node.Name + "/" + node.Kind + "/" + node.SideEffect, After: otherNode.Name + "/" + otherNode.Kind + "/" + otherNode.SideEffect, Kind: DiffBehavioral, Explanation: "node identity or execution class changed"})
	}
	if !reflect.DeepEqual(node.Inputs, otherNode.Inputs) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].inputs", Before: fmt.Sprintf("%v", node.Inputs), After: fmt.Sprintf("%v", otherNode.Inputs), Kind: DiffBehavioral, Explanation: "node inputs changed"})
	}
	if !reflect.DeepEqual(node.Outputs, otherNode.Outputs) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].outputs", Before: fmt.Sprintf("%v", node.Outputs), After: fmt.Sprintf("%v", otherNode.Outputs), Kind: DiffBehavioral, Explanation: "node outputs changed"})
	}
	if node.Timeout != otherNode.Timeout {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].timeout", Before: fmt.Sprintf("%d", node.Timeout), After: fmt.Sprintf("%d", otherNode.Timeout), Kind: DiffBehavioral, Explanation: "node timeout changed"})
	}
	if !reflect.DeepEqual(node.Retry, otherNode.Retry) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].retry", Before: fmt.Sprintf("%v", node.Retry), After: fmt.Sprintf("%v", otherNode.Retry), Kind: DiffBehavioral, Explanation: "node retry policy changed"})
	}
	if !reflect.DeepEqual(node.Resources, otherNode.Resources) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].resources", Before: fmt.Sprintf("%v", node.Resources), After: fmt.Sprintf("%v", otherNode.Resources), Kind: DiffBehavioral, Explanation: "node resource requirements changed"})
	}
	if !reflect.DeepEqual(node.Parallelism, otherNode.Parallelism) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].parallelism", Before: fmt.Sprintf("%v", node.Parallelism), After: fmt.Sprintf("%v", otherNode.Parallelism), Kind: DiffBehavioral, Explanation: "node parallelism settings changed"})
	}
	if node.HumanGate != otherNode.HumanGate {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].humanGate", Before: fmt.Sprintf("%t", node.HumanGate), After: fmt.Sprintf("%t", otherNode.HumanGate), Kind: DiffBehavioral, Explanation: "node human gate requirement changed"})
	}
	if !taskEquivalent(node.Task, otherNode.Task) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].task", Before: fmt.Sprintf("%v", node.Task), After: fmt.Sprintf("%v", otherNode.Task), Kind: DiffBehavioral, Explanation: "task definition changed"})
	}
	if !gateEquivalent(node.Gate, otherNode.Gate) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].gate", Before: fmt.Sprintf("%v", node.Gate), After: fmt.Sprintf("%v", otherNode.Gate), Kind: DiffBehavioral, Explanation: "gate definition changed"})
	}
	if !parallelEquivalent(node.Parallel, otherNode.Parallel) {
		*changes = append(*changes, Change{Path: "nodes[" + node.Name + "].parallel", Before: fmt.Sprintf("%v", node.Parallel), After: fmt.Sprintf("%v", otherNode.Parallel), Kind: DiffBehavioral, Explanation: "parallel definition changed"})
	}
}

func appendEdgeDiffs(changes *[]Change, before, after []Edge) {
	if sameEdgeSet(before, after) {
		return
	}
	if len(before) != len(after) {
		for i := 0; i < len(before); i++ {
			if !containsEdge(after, before[i]) {
				*changes = append(*changes, Change{Path: fmt.Sprintf("edges[%d]", i), Before: formatEdge(before[i]), After: "<none>", Kind: DiffBehavioral, Explanation: "transition edge removed"})
			}
		}
		for i := 0; i < len(after); i++ {
			if !containsEdge(before, after[i]) {
				*changes = append(*changes, Change{Path: fmt.Sprintf("edges[%d]", i), Before: "<none>", After: formatEdge(after[i]), Kind: DiffBehavioral, Explanation: "transition edge added"})
			}
		}
		return
	}
	for i := 0; i < len(before); i++ {
		if !containsEdge(after, before[i]) {
			*changes = append(*changes, Change{Path: fmt.Sprintf("edges[%d]", i), Before: formatEdge(before[i]), After: "<none>", Kind: DiffBehavioral, Explanation: "transition behavior changed"})
		}
	}
}

func sameStringSet(a, b []string) bool {
	return reflect.DeepEqual(normalizeStringSlice(a), normalizeStringSlice(b))
}

func sameSchemas(a, b []Schema) bool {
	if len(a) != len(b) {
		return false
	}
	canonA := make([]string, 0, len(a))
	for _, s := range a {
		canonA = append(canonA, canonicalSchema(s))
	}
	canonB := make([]string, 0, len(b))
	for _, s := range b {
		canonB = append(canonB, canonicalSchema(s))
	}
	sort.Strings(canonA)
	sort.Strings(canonB)
	return reflect.DeepEqual(canonA, canonB)
}

func sameTriggers(a, b []apiv1.Trigger) bool {
	if len(a) != len(b) {
		return false
	}
	canonA := make([]string, 0, len(a))
	for _, tr := range a {
		canonA = append(canonA, canonicalTrigger(tr))
	}
	canonB := make([]string, 0, len(b))
	for _, tr := range b {
		canonB = append(canonB, canonicalTrigger(tr))
	}
	sort.Strings(canonA)
	sort.Strings(canonB)
	return reflect.DeepEqual(canonA, canonB)
}

func sameEdgeSet(a, b []Edge) bool {
	if len(a) != len(b) {
		return false
	}
	canonA := make([]string, 0, len(a))
	for _, edge := range a {
		canonA = append(canonA, canonicalEdge(edge))
	}
	canonB := make([]string, 0, len(b))
	for _, edge := range b {
		canonB = append(canonB, canonicalEdge(edge))
	}
	sort.Strings(canonA)
	sort.Strings(canonB)
	return reflect.DeepEqual(canonA, canonB)
}

func containsEdge(edges []Edge, target Edge) bool {
	for _, edge := range edges {
		if canonicalEdge(edge) == canonicalEdge(target) {
			return true
		}
	}
	return false
}

func normalizeStringSlice(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func canonicalTask(task *apiv1.Task) *apiv1.Task {
	if task == nil {
		return nil
	}
	clone := task.DeepCopy()
	clone.Capabilities = normalizeStringSlice(clone.Capabilities)
	clone.ContextFrom = normalizeStringSlice(clone.ContextFrom)
	clone.PolicyActions = normalizeStringSlice(clone.PolicyActions)
	clone.RequiredCapabilities = normalizeStringSlice(clone.RequiredCapabilities)
	clone.ExpectedOutputs = normalizeStringSlice(clone.ExpectedOutputs)
	clone.Outbox = normalizeStringSlice(clone.Outbox)
	return clone
}

func taskEquivalent(a, b *apiv1.Task) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(canonicalTask(a), canonicalTask(b))
}

func gateEquivalent(a, b *apiv1.Gate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	cloneA := a.DeepCopy()
	cloneB := b.DeepCopy()
	if cloneA.Human != nil {
		cloneA.Human.Approvers = normalizeStringSlice(cloneA.Human.Approvers)
	}
	if cloneB.Human != nil {
		cloneB.Human.Approvers = normalizeStringSlice(cloneB.Human.Approvers)
	}
	return reflect.DeepEqual(cloneA, cloneB)
}

func parallelEquivalent(a, b *apiv1.Parallel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

func canonicalSchema(s Schema) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return s.Name
	}
	return string(raw)
}

func canonicalTrigger(tr apiv1.Trigger) string {
	tr.Events = normalizeStringSlice(tr.Events)
	raw, err := json.Marshal(tr)
	if err != nil {
		return fmt.Sprintf("%v", tr)
	}
	return string(raw)
}

func canonicalEdge(edge Edge) string {
	return edge.From + "|" + edge.Condition + "|" + edge.To
}

func formatEdge(edge Edge) string {
	return edge.From + "->" + edge.To + "[" + edge.Condition + "]"
}

func hasBehavioralChange(changes []Change) bool {
	for _, change := range changes {
		if change.Kind == DiffBehavioral {
			return true
		}
	}
	return false
}

// ExplainLoss records the unavoidable information loss when a workflow source is
// converted to canonical IR and back to a generated representation.
func (d Document) ExplainLoss(source any) []Loss {
	loss := make([]Loss, 0, 2)
	switch src := source.(type) {
	case workflow.Definition:
		loss = append(loss, explainDefinitionLoss(src, d)...)
	case apiv1.Workflow:
		loss = append(loss, explainWorkflowLoss(src, d)...)
	case nil:
		return nil
	}
	return loss
}

func explainDefinitionLoss(src workflow.Definition, doc Document) []Loss {
	loss := make([]Loss, 0, 2)
	digest, err := workflow.ComputeDigest(src)
	if err != nil || digest != doc.Source.Digest {
		loss = append(loss, Loss{Field: "source.digest", Before: digest, After: doc.Source.Digest, Explanation: "the persisted source does not match the IR source digest"})
		return loss
	}
	if doc.SourceDefinition == nil {
		loss = append(loss, Loss{Field: "source.definition", Before: "workflow definition", After: "not persisted", Explanation: "the IR predates source-definition persistence, so exact round-trip fidelity cannot be established"})
		return loss
	}
	if !reflect.DeepEqual(src, *doc.SourceDefinition) {
		loss = append(loss, Loss{Field: "source.definition", Before: "original workflow definition", After: "persisted workflow definition", Explanation: "the persisted source differs from the supplied definition"})
	}
	return loss
}

func explainWorkflowLoss(src apiv1.Workflow, doc Document) []Loss {
	loss := make([]Loss, 0, 2)
	if src.Name != doc.Source.Name {
		loss = append(loss, Loss{Field: "source.name", Before: src.Name, After: doc.Source.Name, Explanation: "the supplied source does not identify the persisted IR source"})
	}
	if doc.SourceDefinition == nil {
		loss = append(loss, Loss{Field: "source.definition", Before: src.Name, After: "not persisted", Explanation: "the IR does not retain the original workflow definition needed to establish fidelity for this workflow source"})
		return loss
	}
	if doc.SourceDefinition.Name != src.Name {
		loss = append(loss, Loss{Field: "source.name", Before: src.Name, After: doc.SourceDefinition.Name, Explanation: "the supplied workflow name does not match the persisted source definition"})
	}
	if doc.SourceDefinition.DSLVersion != src.DSLVersion {
		loss = append(loss, Loss{Field: "source.dslVersion", Before: src.DSLVersion, After: doc.SourceDefinition.DSLVersion, Explanation: "the supplied workflow DSL version does not match the persisted source record"})
	}
	if !reflect.DeepEqual(src.Spec, doc.SourceDefinition.Spec) {
		loss = append(loss, Loss{Field: "source.spec", Before: "supplied workflow spec", After: "persisted workflow spec", Explanation: "the supplied workflow spec differs from the persisted source definition"})
	}
	return loss
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
