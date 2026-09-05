// Package v30 implements the DSL 3.0 workflow interpreter — the Goobernetes
// language surface (docs/design/dsl-3.0.md): stage-level runsOn placement
// requirements, declared repoFrom repo-handoff edges (WF022, reaching
// definitions), and the restrictions vocabulary. Copied forward from the
// frozen 2.0 interpreter (internal/workflow/v_2_0) per dsl-3.0.md §8/D18.
//
// This package owns every version-observable rule from parsed API fields to a
// compiled machine. It is copied forward when a new DSL interpreter is cut.
//
// Naming (dsl-3.0.md open point 1): version-literal — directory v_3_0,
// package v30. The relative names v_current/v_next stop meaning anything the
// moment three interpreters coexist (they do today: 1.4 lives until #3507
// deletes it), and every future cut under relative naming would force a
// rename churn through two packages. Version-literal names are stable
// forever; #3507 may re-home the older two on the same scheme.
package v30

import "github.com/goobers/goobers/internal/workflow/internal/model"

// DSLVersion is the language version whose semantics this interpreter owns.
const DSLVersion = "3.0"

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
