package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

// Stage-qualified inputsFrom (#562) on the engine walk — plan item E2.
//
// Before this, runTask looked every inputsFrom value up as a LITERAL key in the
// immediately preceding task's Outputs. A qualified "<stage>.<key>" reference
// was therefore never found, and the walk failed the stage closed ("upstream
// output %q not found") on a definition the local runner completes. That was
// the E2-inputsfrom-stage-qualified parity row.
//
// The resolution ORDER is NOT re-implemented here. It is shared from
// internal/runner (runner.ResolveInputsFrom and friends, the #624
// shared-constant pattern), because it decides what an already-released DSL
// version MEANS: a copy that drifted would bind a different value on one runner
// than the other for the same definition, silently. What this file adds is the
// engine-side STATE — the completed-stage map as ordinary workflow state — and
// nothing else.
//
// Determinism: the map is a local of walk, threaded into runTask, holding only
// what a stage reported (bare scalars plus the producing stage's integrity
// grade). It is rebuilt identically on replay from the same deterministic
// sequence of task results, and it is never iterated — only keyed — so it
// introduces no map-ordering nondeterminism into the workflow.

// completedStages is every completed stage's Outputs keyed by stage name.
type completedStages = runner.StageOutputs

// newCompletedStages builds the walk's completed-stage state.
func newCompletedStages() completedStages { return runner.NewStageOutputs() }

// recordCompletedStage applies the runner's own record-or-forget rule to a
// finished task: a TOLERATED failure (ContinueOnError) is forgotten so a
// qualified reference to it falls through to bare-key resolution, exactly as it
// does on the local runner.
//
// This has to be its own step rather than a reuse of the walk's `upstream` map:
// the engine already nils a tolerated failure's Outputs before storing it in
// upstream, so the stage is PRESENT-but-empty there, while the runner's map has
// it ABSENT. Resolving against upstream would make a qualified reference to a
// tolerated failure report "stage produced no output" where the local runner
// falls through to the bare key.
func recordCompletedStage(completed completedStages, t apiv1.Task, result apiv1.ResultEnvelope) {
	completed.RecordCompleted(t, result)
}

// resolveInputsFrom resolves one inputsFrom value against the run's completed
// stage outputs. See runner.ResolveInputsFrom for the rule and why it is shared.
func resolveInputsFrom(value string, upstream apiv1.ResultEnvelope, completed completedStages, qualified bool) (interface{}, bool) {
	return runner.ResolveInputsFrom(value, upstream, completed, qualified)
}

// inputsFromIntegrity grades whatever resolveInputsFrom would bind: the
// PRODUCING stage for a qualified reference (TBH-4 — bare scalars have nowhere
// to carry a label of their own), the immediate upstream for a bare key.
func inputsFromIntegrity(value string, upstream apiv1.ResultEnvelope, completed completedStages, qualified bool) apiv1.Integrity {
	return runner.InputsFromIntegrity(value, upstream, completed, qualified)
}

// inputsFromError phrases an unresolvable reference the way the local runner
// does, including its two distinct messages: a qualified reference to a stage
// that RAN but did not emit the key names what it did emit, while everything
// else keeps the legacy "upstream output %q not found" wording pre-3.0
// operators know.
func inputsFromError(taskName, inputKey, value string, completed completedStages, qualified bool) error {
	return runner.InputsFromError(taskName, inputKey, value, completed, qualified)
}
