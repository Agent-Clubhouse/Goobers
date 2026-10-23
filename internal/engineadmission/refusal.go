// Package engineadmission owns the pure support checks shared by engine starts
// and generated-child validation. It does not schedule workflows or grant authority.
package engineadmission

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	wf "github.com/goobers/goobers/internal/workflow"
)

// R9 (finding 002, decision 005): local-runner behaviours have NO engine
// walk implementation and are not being ported. Every one of them is silently
// ignored by the walk today — a definition declaring them starts, runs, and
// completes as if the declaration were absent, which is the worst possible
// shape for a safety knob (an experiment arm that never splits, an outbox that never exports, a fan-out that never fans).
// These sentinels make each one a REFUSAL instead: loud, named, and testable.
//
// The refusal is applied at the run-start boundary (Registry.StartInputVersion,
// which Registry.StartInput funnels through), NOT at RegisterDefinition.
// That placement is deliberate and load-bearing:
// internal/bootstrap.RegisterGaggleWorkflows registers EVERY workflow in a
// gaggle into one registry and fails the whole build on the first error, so
// refusing at registration would take a gaggle's entire lane set offline
// because one unrelated lane declares a feature (the goobers gaggle's
// quality-sprint.yaml declares parallels today). Refusing at start scopes the
// blast radius to the run that would actually have been walked wrongly, which
// is the same per-lane containment ruling 4 mandates for the interim.
// The sentinels carry NO "engine: " prefix of their own. UnsupportedFeaturesError
// already opens with "engine: refuse to start workflow %q: ", and a prefix on
// both produced the doubled reading an operator sees off `goobers engine-start`:
//
//	engine: refuse to start workflow "quality-sprint": declares spec.parallels: engine: spec.parallels ...
//
// Each sentence below therefore reads correctly standalone (errors.Is callers
// printing the sentinel) and wrapped (the refusal an operator actually reads).
var (
	// ErrExperimentUnsupported refuses task.experiment (bandit arm assignment
	// and observation recording: internal/runner/run.go's AssignAndRecord /
	// recordBanditResult arms).
	ErrExperimentUnsupported = errors.New("task.experiment (bandit arm assignment) is not implemented on the engine walk")
	// ErrParallelsUnsupported refuses spec.parallels (internal/runner's
	// parallel_run.go fan-out/fan-in, @join branches and branch-qualified
	// inputs).
	ErrParallelsUnsupported = errors.New("spec.parallels (fan-out/fan-in branches) are not implemented on the engine walk")
	// ErrMapsUnsupported refuses DSL 3.1 spec.maps (data-driven fan-out).
	ErrMapsUnsupported = errors.New("spec.maps (data-driven fan-out) is not implemented on the engine walk")
	// ErrHumanGateUnsupported refuses evaluator: human until the engine owns
	// the occurrence-bound decision protocol.
	ErrHumanGateUnsupported = errors.New(HumanGateUnsupported)
)

// UnsupportedFeatureError names one refused declaration: which stage (empty for
// a spec-level declaration), which DSL path, and the sentinel it refuses. It
// wraps the sentinel so callers classify with errors.Is instead of string
// matching.
//
// It deliberately carries no Workflow field: the workflow name is the enclosing
// UnsupportedFeaturesError's, which is what prints it and what errors.As reaches
// for the run-level question ("which workflow was refused"). A per-refusal copy
// was set on construction and read by nothing — a field a reader would assume
// was load-bearing.
type UnsupportedFeatureError struct {
	// Stage is the declaring task's or fan-out state's name; empty for a
	// spec-level declaration.
	Stage string
	// Feature is the DSL path of the refused declaration, e.g.
	// "task.experiment" or "spec.parallels".
	Feature string
	// Err is the matching Err*Unsupported sentinel.
	Err error
}

// Error names WHERE the refused declaration is and lets the sentinel say what
// it is. The sentinel already opens with the DSL path, so repeating Feature here
// only produced "declares spec.parallels: spec.parallels (fan-out/fan-in
// branches) are not implemented…". Feature remains the structured field an
// errors.As caller keys on; it is just not said twice in one sentence.
func (e *UnsupportedFeatureError) Error() string {
	if e.Stage != "" {
		return fmt.Sprintf("stage %q: %v", e.Stage, e.Err)
	}
	return e.Err.Error()
}

// Unwrap exposes the sentinel to errors.Is.
func (e *UnsupportedFeatureError) Unwrap() error { return e.Err }

// UnsupportedFeaturesError is the run-start refusal for a definition that
// declares one or more R9 features. Its message names the workflow and every
// refused declaration in one line; errors.Is reaches each sentinel and
// errors.As reaches each *UnsupportedFeatureError through the multi-error
// Unwrap below.
type UnsupportedFeaturesError struct {
	// Workflow is the definition's registered name.
	Workflow string
	// Refusals is every refused declaration, in declaration order.
	Refusals []*UnsupportedFeatureError
}

func (e *UnsupportedFeaturesError) Error() string {
	parts := make([]string, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		parts = append(parts, r.Error())
	}
	return fmt.Sprintf("engine: refuse to start workflow %q: %s", e.Workflow, strings.Join(parts, "; "))
}

// Unwrap returns every refusal so errors.Is/errors.As traverse the whole set
// rather than only the first (Go 1.20 multi-error unwrapping).
func (e *UnsupportedFeaturesError) Unwrap() []error {
	out := make([]error, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		out = append(out, r)
	}
	return out
}

// UnsupportedFeatures returns every R9 declaration in spec, in a stable
// order (spec-level first, then tasks in declaration order, then the per-task
// feature order below). It reports ALL of them rather than the first so an
// operator fixing a definition sees the whole set in one refusal.
func UnsupportedFeatures(spec apiv1.WorkflowSpec) []*UnsupportedFeatureError {
	var out []*UnsupportedFeatureError
	if len(spec.Parallels) > 0 {
		out = append(out, &UnsupportedFeatureError{Feature: "spec.parallels", Err: ErrParallelsUnsupported})
	}
	for _, m := range spec.Maps {
		out = append(out, &UnsupportedFeatureError{Stage: m.Name, Feature: "spec.maps", Err: ErrMapsUnsupported})
	}
	for _, t := range spec.Tasks {
		if t.ChildWorkflows != nil {
			out = append(out, &UnsupportedFeatureError{Stage: t.Name, Feature: "task.childWorkflows", Err: wf.ErrChildWorkflowExecutionUnsupported})
		}
		if t.Experiment != nil {
			out = append(out, &UnsupportedFeatureError{Stage: t.Name, Feature: "task.experiment", Err: ErrExperimentUnsupported})
		}
	}
	return out
}

// RefuseFeatures returns the run-start refusal for spec, or
// nil when the definition declares no R9 feature.
func RefuseFeatures(name string, spec apiv1.WorkflowSpec) error {
	refusals := UnsupportedFeatures(spec)
	if len(refusals) == 0 {
		return nil
	}
	return &UnsupportedFeaturesError{Workflow: name, Refusals: refusals}
}

// RefuseHumanGate is run()'s human-gate refusal, factored out so the daemon's
// pre-dispatch check (RefuseDefinition) cannot drift from the walk's own.
func RefuseHumanGate(g apiv1.Gate) error {
	if g.Evaluator != apiv1.EvaluatorHuman {
		return nil
	}
	return fmt.Errorf("%w: gate %q", ErrHumanGateUnsupported, g.Name)
}

// RefuseDefinition reports why the engine walk cannot execute this definition,
// or nil when it can. It aggregates every refusal the walk applies from the
// definition ALONE — the R9 unsupported-feature set and human gates.
//
// # Why the daemon needs this before it dispatches
//
// Both refusals fire late: R9 at Registry.StartInputVersion, human gates
// inside run() itself, and the human-gate one lands BEFORE newRunJournal, so
// the workflow emits nothing whatsoever. That placement is right for
// `goobers engine-start`, where the operator sees the refusal on stderr and
// fixes the definition.
//
// It is wrong as a scheduler outcome. Since #3876 a cron tick can dispatch a
// lane onto the engine, and neither refusal is visible to the placement-pin
// predicate that made that choice: `spec.parallels`, `task.experiment`,
// `task.limits`, and human gates contribute no placement rows,
// so a fully remote-pinned lane declaring any of them is selected for the
// engine and then refused on EVERY tick — permanently, having run perfectly
// well on the local runner the day before.
//
// So the daemon asks this question at selection time and keeps such a lane on
// the runner, where the feature is implemented, with the refusal text as the
// operator-facing reason.
func RefuseDefinition(name string, spec apiv1.WorkflowSpec) error {
	var refusals []*UnsupportedFeatureError
	var declared *UnsupportedFeaturesError
	if err := RefuseFeatures(name, spec); err != nil {
		if !errors.As(err, &declared) {
			return err
		}
		refusals = append(refusals, declared.Refusals...)
	}
	for _, g := range spec.Gates {
		if err := RefuseHumanGate(g); err != nil {
			refusals = append(refusals, &UnsupportedFeatureError{
				Stage: g.Name, Feature: "gate.evaluator.human", Err: err,
			})
		}
	}
	return refusalError(name, refusals)
}

// RefusePlacedDefinition also checks features whose support depends on the
// pinned executor. Remote usage limits are supported through trusted
// supervisor surrender usage; placements remain for future checks.
func RefusePlacedDefinition(name string, spec apiv1.WorkflowSpec, _ []dispatcher.PinnedPlacement) error {
	refusals, err := placedDefinitionRefusals(name, spec, RefuseDefinition)
	if err != nil {
		return err
	}
	return refusalError(name, refusals)
}

func placedDefinitionRefusals(
	name string,
	spec apiv1.WorkflowSpec,
	refuseDefinition func(string, apiv1.WorkflowSpec) error,
) ([]*UnsupportedFeatureError, error) {
	var refusals []*UnsupportedFeatureError
	var declared *UnsupportedFeaturesError
	err := refuseDefinition(name, spec)
	if err != nil && !errors.As(err, &declared) {
		return nil, err
	}
	if declared != nil {
		refusals = append(refusals, declared.Refusals...)
	}
	return refusals, nil
}

func refusalError(name string, refusals []*UnsupportedFeatureError) error {
	if len(refusals) == 0 {
		return nil
	}
	return &UnsupportedFeaturesError{Workflow: name, Refusals: refusals}
}

// HumanGateUnsupported is retained verbatim for historical engine refusals.
const HumanGateUnsupported = "engine: human gates require occurrence-bound Temporal signals and are not supported yet"
