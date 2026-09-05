// Package workflow routes pinned workflow definitions to versioned DSL
// interpreters and exposes the version-invariant machine contract runners walk.
//
// The shared boundary is intentionally narrow: Definition and Machine
// run-pinning, the state-machine walk and graph projection, and the digest
// algorithm are shared because they are definitionally version-invariant.
// Everything that assigns meaning to YAML fields, including compilation,
// validation, feature resolution, and execution-policy projection, belongs to
// a versioned interpreter package such as v_2_0. A new DSL version copies
// that interpreter forward rather than changing an older version in place.
package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// Definition is a versioned snapshot pinned for a workflow run.
type Definition = model.Definition

// Machine is the compiled, validated state machine runners execute.
type Machine = model.Machine

// ComputeDigest returns the stable digest of a pinned workflow definition.
func ComputeDigest(def Definition) (string, error) {
	return model.ComputeDigest(def)
}

const (
	// TerminalComplete ends a run successfully.
	TerminalComplete = model.TerminalComplete
	// TargetAbort ends a run as blocked.
	TargetAbort = model.TargetAbort
	// TargetJoin ends a parallel BRANCH. It is reserved but not terminal: the
	// run continues at the parallel's join state once every branch settles.
	TargetJoin = model.TargetJoin
	// TargetEscalate ends a run as needing human intervention.
	TargetEscalate = model.TargetEscalate
	// BranchEscalate routes forced escalation through a workflow branch.
	BranchEscalate = model.BranchEscalate
)

// IsReservedTarget reports whether target is a reserved TERMINAL action
// ("@abort"/"@escalate") — it ends the run. It deliberately excludes "@join",
// which ends a BRANCH and continues the run at the join state.
//
// Callers asking "must this resolve to a declared state?" want
// IsReservedAnyTarget instead. This narrower form exists for the shipped-
// workflow contract assertions, which check that a failing outcome routes
// through a park stage rather than straight at a terminal (#929) — a question
// only the terminal/branch distinction can answer.
func IsReservedTarget(target string) bool {
	return model.IsReservedTarget(target)
}

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
// even when a stage named "foo" happens to exist. DSL 1.4 is dropped (#3507);
// every remaining live version (2.0, 3.0) supports stage-qualified inputs, but
// the guard keeps naming the one version that did not, by its string, so a
// re-introduced legacy interpreter cannot silently inherit the new meaning.
func SupportsStageQualifiedInputs(m *Machine) bool {
	if m == nil {
		return false
	}
	return m.Def.DSLVersion != supportmatrix.CurrentDSLVersion
}
