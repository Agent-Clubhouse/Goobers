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

// Graph is the canonical executable workflow graph.
type Graph = model.Graph

// GraphEdge is a directed transition between workflow nodes.
type GraphEdge = model.GraphEdge

// GraphNode is one executable or control-flow node in a workflow graph.
type GraphNode = model.GraphNode

// GraphNodeKind classifies executable and control-flow nodes.
type GraphNodeKind = model.GraphNodeKind

const (
	// GraphNodeDeterministic identifies a deterministic task.
	GraphNodeDeterministic = model.GraphNodeDeterministic
	// GraphNodeAgentic identifies an agent-backed task.
	GraphNodeAgentic = model.GraphNodeAgentic
	// GraphNodeGate identifies a policy gate.
	GraphNodeGate = model.GraphNodeGate
	// GraphNodeParallel identifies a parallel fan-out node.
	GraphNodeParallel = model.GraphNodeParallel
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
