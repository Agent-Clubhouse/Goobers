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

const (
	// TerminalComplete ends a run successfully.
	TerminalComplete = model.TerminalComplete
	// TargetAbort ends a run as blocked.
	TargetAbort = model.TargetAbort
	// TargetEscalate ends a run as needing human intervention.
	TargetEscalate = model.TargetEscalate
	// BranchEscalate routes forced escalation through a workflow branch.
	BranchEscalate = model.BranchEscalate
)

func isTerminal(target string) bool {
	return target == TerminalComplete || model.IsReservedTarget(target)
}
