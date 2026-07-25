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
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
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

// SupportsStageQualifiedInputs reports whether a machine's pinned DSL version
// resolves stage-qualified inputsFrom references ("<stage>.<key>", #562).
//
// This gate exists because inputsFrom resolution lives in the RUNNER, which is
// shared by every interpreter — without it, adding the feature would change
// what an already-released DSL version means underneath workflows authored
// against it. That is precisely the silent-semantic-drift DVL exists to
// prevent (docs/design/dsl-version-lifecycle.md §3.1: a field's meaning may
// only change across a MAJOR bump).
//
// Concretely: under DSL 1.4 a value like "foo.bar" is always a bare output key,
// even when a stage named "foo" happens to exist.
func SupportsStageQualifiedInputs(m *Machine) bool {
	if m == nil {
		return false
	}
	return m.Def.DSLVersion != vcurrent.DSLVersion
}
