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

// IsReservedTarget reports whether target is a reserved terminal action.
func IsReservedTarget(target string) bool {
	return model.IsReservedTarget(target)
}

// BranchTarget resolves a gate outcome to its declared transition target.
func BranchTarget(gate apiv1.Gate, outcome string) (target string, ok bool) {
	return model.BranchTarget(gate, outcome)
}
