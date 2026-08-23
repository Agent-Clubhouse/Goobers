// Package vnext implements the next workflow DSL interpreter.
//
// This package owns every version-observable rule from parsed API fields to a
// compiled machine. It is copied forward when a new DSL interpreter is cut.
package vnext

import "github.com/goobers/goobers/internal/workflow/internal/model"

// DSLVersion is the language version whose semantics this interpreter freezes.
const DSLVersion = "2.0"

// Definition is the shared versioned workflow snapshot.
type Definition = model.Definition

// Machine is the shared compiled runtime machine.
type Machine = model.Machine
type Graph = model.Graph
type GraphEdge = model.GraphEdge
type GraphNode = model.GraphNode
type GraphNodeKind = model.GraphNodeKind

const (
	GraphNodeDeterministic = model.GraphNodeDeterministic
	GraphNodeAgentic       = model.GraphNodeAgentic
	GraphNodeGate          = model.GraphNodeGate
	GraphNodeParallel      = model.GraphNodeParallel
)

const (
	// TerminalComplete ends a run successfully.
	TerminalComplete = model.TerminalComplete
	// TargetAbort ends a run as blocked.
	TargetAbort = model.TargetAbort
	// TargetEscalate ends a run as needing human intervention.
	TargetEscalate = model.TargetEscalate
	// BranchEscalate routes forced escalation through a workflow branch.
	BranchEscalate = model.BranchEscalate
	// TargetJoin ends a parallel branch and is reserved but NOT terminal.
	TargetJoin = model.TargetJoin
)

// isTerminal reports whether a target ends the run. TargetJoin is deliberately
// excluded: it ends a BRANCH and the run continues at the join state, so
// treating it as terminal would let a branch satisfy the canExit fixed point
// while never reaching a real exit.
func isTerminal(target string) bool {
	return target == TerminalComplete || model.IsReservedTarget(target)
}

// isStateName reports whether a target must resolve to a declared state rather
// than being a reserved word. Use it for dangling-reference checks.
func isStateName(target string) bool {
	return !isTerminal(target) && !model.IsReservedBranchTarget(target)
}
