package engine

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// R9 (finding 002, decision 005): four local-runner behaviours have NO engine
// walk implementation and are not being ported. Every one of them is silently
// ignored by the walk today — a definition declaring them starts, runs, and
// completes as if the declaration were absent, which is the worst possible
// shape for a safety knob (an experiment arm that never splits, a token budget
// that never caps, an outbox that never exports, a fan-out that never fans).
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
	// ErrUsageLimitsUnsupported refuses task.limits.maxTokens /
	// task.limits.maxCostUSD (the local runner's cumulative
	// enforceStageBudget). MaxDurationSeconds is NOT refused — the engine
	// enforces it through the stage activity's StartToCloseTimeout.
	ErrUsageLimitsUnsupported = errors.New("task.limits.maxTokens/maxCostUSD (cumulative agentic usage budgets) are not implemented on the engine walk")
	// ErrOutboxUnsupported refuses task.outbox (internal/runner/outbox.go's
	// workspace-relative export, #1552).
	ErrOutboxUnsupported = errors.New("task.outbox (workspace file export) is not implemented on the engine walk")
	// ErrParallelsUnsupported refuses spec.parallels (internal/runner's
	// parallel_run.go fan-out/fan-in, @join branches and branch-qualified
	// inputs).
	ErrParallelsUnsupported = errors.New("spec.parallels (fan-out/fan-in branches) are not implemented on the engine walk")
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
	// Stage is the declaring task's name; empty for a spec-level declaration.
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

// unsupportedEngineFeatures returns every R9 declaration in spec, in a stable
// order (spec-level first, then tasks in declaration order, then the per-task
// feature order below). It reports ALL of them rather than the first so an
// operator fixing a definition sees the whole set in one refusal.
func unsupportedEngineFeatures(spec apiv1.WorkflowSpec) []*UnsupportedFeatureError {
	var out []*UnsupportedFeatureError
	if len(spec.Parallels) > 0 {
		out = append(out, &UnsupportedFeatureError{Feature: "spec.parallels", Err: ErrParallelsUnsupported})
	}
	for _, t := range spec.Tasks {
		if t.Experiment != nil {
			out = append(out, &UnsupportedFeatureError{Stage: t.Name, Feature: "task.experiment", Err: ErrExperimentUnsupported})
		}
		if t.Limits != nil && (t.Limits.MaxTokens > 0 || t.Limits.MaxCostUSD > 0) {
			out = append(out, &UnsupportedFeatureError{Stage: t.Name, Feature: "task.limits.maxTokens/maxCostUSD", Err: ErrUsageLimitsUnsupported})
		}
		if len(t.Outbox) > 0 {
			out = append(out, &UnsupportedFeatureError{Stage: t.Name, Feature: "task.outbox", Err: ErrOutboxUnsupported})
		}
	}
	return out
}

// refuseUnsupportedEngineFeatures returns the run-start refusal for spec, or
// nil when the definition declares no R9 feature.
func refuseUnsupportedEngineFeatures(name string, spec apiv1.WorkflowSpec) error {
	refusals := unsupportedEngineFeatures(spec)
	if len(refusals) == 0 {
		return nil
	}
	return &UnsupportedFeaturesError{Workflow: name, Refusals: refusals}
}
