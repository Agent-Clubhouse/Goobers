// Package model contains the workflow runtime contract shared by every DSL
// interpreter.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Reserved transition targets. A normal terminal transition is the empty string
// (the run completes). Gates may also route a (typically failing) outcome to one
// of these reserved targets to end the run with a non-completed status — a
// defined branch, never a silent pass (GT-002).
const (
	// TerminalComplete ends the run successfully.
	TerminalComplete = ""
	// TargetAbort ends the run as blocked (e.g. a review rejected the work).
	TargetAbort = "@abort"
	// TargetEscalate ends the run as needing escalation/human intervention.
	TargetEscalate = "@escalate"
	// BranchEscalate optionally routes a runner-forced escalation through a
	// workflow state before termination.
	BranchEscalate = "escalate"
)

// IsReservedTarget reports whether a transition target is a reserved terminal
// action rather than a state name.
func IsReservedTarget(target string) bool {
	return target == TargetAbort || target == TargetEscalate
}

// Definition is a versioned snapshot of a workflow definition. A run pins one of
// these for its entire life (WF-016). Version is the registry-assigned monotonic
// version; the content digest (Machine.Digest) is the tamper-evident pin.
type Definition struct {
	Name       string
	Version    int
	DSLVersion string `json:"dslVersion,omitempty"`
	Spec       apiv1.WorkflowSpec
}

// Machine is a compiled, validated view of a Definition with O(1) state lookup
// and a stable content digest.
type Machine struct {
	Def    Definition
	tasks  map[string]apiv1.Task
	gates  map[string]apiv1.Gate
	graph  Graph
	digest string
}

// NewMachine stores interpreter-built runtime state and atomically pins its
// definition digest.
func NewMachine(def Definition, tasks map[string]apiv1.Task, gates map[string]apiv1.Gate, graph Graph) (*Machine, error) {
	digest, err := ComputeDigest(def)
	if err != nil {
		return nil, err
	}
	graph.Name = def.Name
	graph.Version = def.Version
	graph.Digest = digest
	return &Machine{
		Def:    def,
		tasks:  tasks,
		gates:  gates,
		graph:  cloneGraph(graph),
		digest: digest,
	}, nil
}

// Digest returns the content digest of the compiled definition ("sha256:<hex>").
// It is stable across processes and runs: the same definition always digests to
// the same value, so a run can record and complete on a pinned digest (WF-016).
func (m *Machine) Digest() string { return m.digest }

// ComputeDigest returns a stable digest of the pinned definition.
func ComputeDigest(def Definition) (string, error) {
	b, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Has reports whether name identifies a task or gate.
func (m *Machine) Has(name string) bool {
	if _, ok := m.tasks[name]; ok {
		return true
	}
	_, ok := m.gates[name]
	return ok
}

// Outgoing returns the transition targets of a state.
func (m *Machine) Outgoing(state string) []string {
	if task, ok := m.tasks[state]; ok {
		return []string{task.Next}
	}
	if gate, ok := m.gates[state]; ok {
		outcomes := make([]string, 0, len(gate.Branches))
		for outcome := range gate.Branches {
			outcomes = append(outcomes, outcome)
		}
		sort.Strings(outcomes)
		targets := make([]string, 0, len(outcomes))
		for _, outcome := range outcomes {
			targets = append(targets, gate.Branches[outcome])
		}
		return targets
	}
	return nil
}

// Task returns the task with the given name, if any.
func (m *Machine) Task(name string) (apiv1.Task, bool) {
	t, ok := m.tasks[name]
	return t, ok
}

// Gate returns the gate with the given name, if any.
func (m *Machine) Gate(name string) (apiv1.Gate, bool) {
	g, ok := m.gates[name]
	return g, ok
}

// BranchTarget resolves a gate outcome to its next transition target. ok is
// false when the gate declares no branch for that outcome — the engine treats
// that as an error (a gate must never silently pass; GT-002).
func BranchTarget(g apiv1.Gate, outcome string) (target string, ok bool) {
	target, ok = g.Branches[outcome]
	return target, ok
}
