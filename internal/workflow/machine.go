// Package workflow is the substrate-neutral core of the Goobers workflow engine:
// the definition compiler and the compiled state machine the runners walk. It is
// extracted from internal/engine per docs/ARCHITECTURE.md §11 so both the local
// runner (V0) and the Temporal runner (V2) execute the *same* compiled machine —
// the Temporal workflow function is a thin adapter around this package.
//
// This package has no runner, no Temporal, and no I/O dependencies (only
// api/v1alpha1 + stdlib). Compilation is pure and deterministic: the same
// definition always yields the same machine and the same content digest
// (WF-002, WF-016).
package workflow

import (
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

// isTerminal reports whether a transition target ends the run (either the
// success terminal or a reserved terminal action).
func isTerminal(target string) bool {
	return target == TerminalComplete || IsReservedTarget(target)
}

// Definition is a versioned snapshot of a workflow definition. A run pins one of
// these for its entire life (WF-016). Version is the registry-assigned monotonic
// version; the content digest (Machine.Digest) is the tamper-evident pin.
type Definition struct {
	Name    string
	Version int
	Spec    apiv1.WorkflowSpec
}

// Machine is a compiled, validated view of a Definition with O(1) state lookup
// and a stable content digest.
type Machine struct {
	Def    Definition
	tasks  map[string]apiv1.Task
	gates  map[string]apiv1.Gate
	digest string
}

// Digest returns the content digest of the compiled definition ("sha256:<hex>").
// It is stable across processes and runs: the same definition always digests to
// the same value, so a run can record and complete on a pinned digest (WF-016).
func (m *Machine) Digest() string { return m.digest }

func (m *Machine) has(name string) bool {
	if _, ok := m.tasks[name]; ok {
		return true
	}
	_, ok := m.gates[name]
	return ok
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
