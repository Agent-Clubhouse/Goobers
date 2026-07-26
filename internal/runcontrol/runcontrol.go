// Package runcontrol resolves inherited workflow run-control policy.
package runcontrol

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	DefaultMaxRepasses = 3
)

const DefaultStalledRunTimeout = 45 * time.Minute

// Effective is a fully resolved run-control policy.
type Effective struct {
	MaxRepasses       int
	StalledRunTimeout time.Duration
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
		if scope.controls.MaxRepasses > 0 {
			effective.MaxRepasses = int(scope.controls.MaxRepasses)
		}
		if scope.controls.StalledRunTimeout != "" {
			effective.StalledRunTimeout, _ = time.ParseDuration(scope.controls.StalledRunTimeout)
		}
	}
	return effective, nil
}

// Validate checks one override block independently of its inheritance parent.
func Validate(path string, controls apiv1.RunControls) error {
	if controls.MaxRepasses < 0 {
		return fmt.Errorf("%s.maxRepasses must be positive, got %d", path, controls.MaxRepasses)
	}
	if controls.StalledRunTimeout == "" {
		return nil
	}
	timeout, err := time.ParseDuration(controls.StalledRunTimeout)
	if err != nil {
		return fmt.Errorf("%s.stalledRunTimeout %q: %w", path, controls.StalledRunTimeout, err)
	}
	if timeout <= 0 {
		return fmt.Errorf("%s.stalledRunTimeout must be positive, got %s", path, timeout)
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
	}
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
