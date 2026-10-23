package config

import (
	"errors"
	"fmt"
)

// ErrNoAppliedGeneration means no configuration generation has been applied.
var ErrNoAppliedGeneration = errors.New("cloud configuration: no generation is applied")

// AdmitApplied checks gaggle/workflow against ONE snapshot of the applied
// generation and returns that generation's digest, so the digest and the
// eligibility verdict can never come from two different generations. Before
// the first apply it wraps ErrNoAppliedGeneration; for a workflow the
// generation did not assess eligible it wraps ErrIneligible.
func (c *Configuration) AdmitApplied(gaggle, workflow string) (string, error) {
	gen := c.applier.Guard().current.Load()
	if gen == nil {
		return "", fmt.Errorf("%w: %w", ErrIneligible, ErrNoAppliedGeneration)
	}
	if _, ok := gen.eligible[workflowKey(gaggle, workflow)]; !ok {
		return "", fmt.Errorf("%w: workflow %s was not assessed eligible in generation %q", ErrIneligible, workflowKey(gaggle, workflow), gen.digest)
	}
	return gen.digest, nil
}
