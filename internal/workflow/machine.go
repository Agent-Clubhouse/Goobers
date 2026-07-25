// Package workflow routes pinned workflow definitions to versioned DSL
// interpreters and exposes the version-invariant machine contract runners walk.
//
// The shared boundary is intentionally narrow: Definition and Machine
// run-pinning, the state-machine walk and graph projection, and the digest
// algorithm are shared because they are definitionally version-invariant.
// Everything that assigns meaning to YAML fields, including compilation,
// validation, feature resolution, and execution-policy projection, belongs to
// a versioned interpreter package such as v_current. A new DSL version copies
// that interpreter forward rather than changing an older version in place.
package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// Definition is a versioned snapshot pinned for a workflow run.
type Definition = model.Definition

// Machine is the compiled, validated state machine runners execute.
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

// IsReservedAnyTarget reports whether target is reserved in any sense —
// a terminal action ("@abort"/"@escalate") or a branch action ("@join").
// Use it where the question is "is this a state name?", which is what every
// dangling-reference check outside the interpreters actually asks.
//
// The narrower model.IsReservedTarget / model.IsReservedBranchTarget are
// deliberately NOT re-exported here: the distinction between "ends the run"
// and "ends a branch" only matters inside an interpreter's reachability
// analysis, and exposing it invites a caller to pick the wrong one.
func IsReservedAnyTarget(target string) bool {
	return model.IsReservedAnyTarget(target)
}

// BranchTarget resolves a gate outcome to its declared transition target.
func BranchTarget(gate apiv1.Gate, outcome string) (target string, ok bool) {
	return model.BranchTarget(gate, outcome)
}
