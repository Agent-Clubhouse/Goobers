// Package runcontrol resolves inherited workflow run-control policy.
package runcontrol

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// DefaultMaxRepasses is the fallback bounded-repass budget.
	DefaultMaxRepasses = 3
)

// DefaultStalledRunTimeout is the fallback inactivity threshold for a run.
const DefaultStalledRunTimeout = 45 * time.Minute

// Effective is a fully resolved run-control policy.
type Effective struct {
	MaxRepasses       int
	StalledRunTimeout time.Duration
	MaxRunDuration    time.Duration
}

// Resolve applies instance defaults, then gaggle and workflow overrides.
func Resolve(instance apiv1.RunControls, gaggle, workflow *apiv1.RunControls) (Effective, error) {
	effective := Effective{
		MaxRepasses:       DefaultMaxRepasses,
		StalledRunTimeout: DefaultStalledRunTimeout,
	}
	for _, scope := range []struct {
		name     string
		controls *apiv1.RunControls
	}{
		{name: "runConditions", controls: &instance},
		{name: "gaggle.spec.runControls", controls: gaggle},
		{name: "workflow.spec.runControls", controls: workflow},
	} {
		if scope.controls == nil {
			continue
		}
		if err := Validate(scope.name, *scope.controls); err != nil {
			return Effective{}, err
		}
		if err := effective.apply(scope.name, *scope.controls); err != nil {
			return Effective{}, err
		}
	}
	return effective, nil
}

// apply layers one scope's overrides onto the effective policy. Parse failures
// propagate: this path used to discard them, so an invalid duration silently
// resolved to zero — an unlimited run — instead of erroring (fail-open).
// Validate screens every scope before apply runs, but the watchdog budget must
// not depend on that call ordering to stay bounded.
func (effective *Effective) apply(path string, controls apiv1.RunControls) error {
	if controls.MaxRepasses > 0 {
		effective.MaxRepasses = int(controls.MaxRepasses)
	}
	if controls.StalledRunTimeout != "" {
		parsed, err := parseDurationField(path, "stalledRunTimeout", controls.StalledRunTimeout)
		if err != nil {
			return err
		}
		effective.StalledRunTimeout = parsed
	}
	if controls.MaxRunDuration != "" {
		parsed, err := parseDurationField(path, "maxRunDuration", controls.MaxRunDuration)
		if err != nil {
			return err
		}
		effective.MaxRunDuration = parsed
	}
	return nil
}

// parseDurationField translates a time.ParseDuration failure into an
// author-facing diagnostic naming the field path, the offending value, and the
// expected form. The raw Go error ("time: invalid duration ...") is an
// implementation detail that must not leak into the DSL contract.
func parseDurationField(path, field, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s.%s %q is not a valid duration; use Go duration syntax, e.g. \"45m\" or \"2h\"", path, field, value)
	}
	return parsed, nil
}

// Validate checks one override block independently of its inheritance parent.
func Validate(path string, controls apiv1.RunControls) error {
	if controls.MaxRepasses < 0 {
		return fmt.Errorf("%s.maxRepasses must be positive, got %d", path, controls.MaxRepasses)
	}
	for _, duration := range []struct {
		name  string
		value string
	}{
		{name: "stalledRunTimeout", value: controls.StalledRunTimeout},
		{name: "maxRunDuration", value: controls.MaxRunDuration},
	} {
		if duration.value == "" {
			continue
		}
		parsed, err := parseDurationField(path, duration.name, duration.value)
		if err != nil {
			return err
		}
		if parsed <= 0 {
			return fmt.Errorf("%s.%s must be positive, got %s", path, duration.name, parsed)
		}
	}
	return nil
}

// ValidatePinned requires a complete effective policy, while nil remains the
// backward-compatible representation of a legacy run.
func ValidatePinned(controls *apiv1.RunControls) error {
	if controls == nil {
		return nil
	}
	if controls.MaxRepasses <= 0 {
		return fmt.Errorf("runControls.maxRepasses must be positive, got %d", controls.MaxRepasses)
	}
	if controls.StalledRunTimeout == "" {
		return fmt.Errorf("runControls.stalledRunTimeout is required")
	}
	return Validate("runControls", *controls)
}

// Overrides returns effective as a fully populated, serializable policy.
func (effective Effective) Overrides() apiv1.RunControls {
	return apiv1.RunControls{
		MaxRepasses:       int32(effective.MaxRepasses),
		StalledRunTimeout: effective.StalledRunTimeout.String(),
		MaxRunDuration:    durationString(effective.MaxRunDuration),
	}
}

func durationString(duration time.Duration) string {
	if duration <= 0 {
		return ""
	}
	return duration.String()
}

// MaxRepassesForGate applies the optional per-gate leaf override.
func MaxRepassesForGate(gate apiv1.Gate, inherited int) int {
	if gate.MaxRepasses > 0 {
		return int(gate.MaxRepasses)
	}
	if inherited > 0 {
		return inherited
	}
	return DefaultMaxRepasses
}

// ValidateWorkflow checks workflow and gate leaf overrides.
func ValidateWorkflow(spec apiv1.WorkflowSpec) error {
	if spec.RunControls != nil {
		if err := Validate("spec.runControls", *spec.RunControls); err != nil {
			return err
		}
	}
	for _, gate := range spec.Gates {
		if gate.MaxRepasses < 0 {
			return fmt.Errorf("gate %q maxRepasses must be positive, got %d", gate.Name, gate.MaxRepasses)
		}
		if gate.MaxRepasses > 0 && gate.Evaluator == apiv1.EvaluatorHuman {
			return fmt.Errorf("gate %q maxRepasses is only valid for automated or agentic gates", gate.Name)
		}
	}
	return nil
}
