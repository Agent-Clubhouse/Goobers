package engine

import (
	"fmt"
	"strings"

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
)

// IsReservedTarget reports whether a transition target is a reserved terminal
// action rather than a state name.
func IsReservedTarget(target string) bool {
	return target == TargetAbort || target == TargetEscalate
}

// Definition is a versioned snapshot of a workflow definition. A run pins one of
// these for its entire life.
type Definition struct {
	Name    string
	Version int
	Spec    apiv1.WorkflowSpec
}

// Machine is a compiled, validated view of a Definition with O(1) state lookup.
type Machine struct {
	Def   Definition
	tasks map[string]apiv1.Task
	gates map[string]apiv1.Gate
}

// Compile validates a Definition's state machine and returns a Machine for
// execution. It checks that state names are unique, the start state exists, and
// every transition target resolves to a defined state or a terminal/reserved
// target. It is pure (no Temporal, no I/O) and is the engine's fail-fast guard
// on top of config-time validation.
func Compile(def Definition) (*Machine, error) {
	m := &Machine{
		Def:   def,
		tasks: make(map[string]apiv1.Task, len(def.Spec.Tasks)),
		gates: make(map[string]apiv1.Gate, len(def.Spec.Gates)),
	}
	var problems []string

	for _, t := range def.Spec.Tasks {
		if m.has(t.Name) {
			problems = append(problems, fmt.Sprintf("duplicate state %q", t.Name))
		}
		m.tasks[t.Name] = t
	}
	for _, g := range def.Spec.Gates {
		if m.has(g.Name) {
			problems = append(problems, fmt.Sprintf("duplicate state %q", g.Name))
		}
		m.gates[g.Name] = g
	}

	if def.Spec.Start == TerminalComplete {
		problems = append(problems, "start state is empty")
	} else if !m.has(def.Spec.Start) {
		problems = append(problems, fmt.Sprintf("start state %q is not defined", def.Spec.Start))
	}

	for _, t := range def.Spec.Tasks {
		if t.Next != TerminalComplete && !IsReservedTarget(t.Next) && !m.has(t.Next) {
			problems = append(problems, fmt.Sprintf("task %q next state %q is not defined", t.Name, t.Next))
		}
	}
	for _, g := range def.Spec.Gates {
		if len(g.Branches) == 0 {
			problems = append(problems, fmt.Sprintf("gate %q has no branches", g.Name))
		}
		for outcome, target := range g.Branches {
			if target != TerminalComplete && !IsReservedTarget(target) && !m.has(target) {
				problems = append(problems, fmt.Sprintf("gate %q branch %q -> %q is not a defined state", g.Name, outcome, target))
			}
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	return m, nil
}

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
