package config

import (
	"context"
	"errors"
	"fmt"
)

// Pins are one workflow's admission digests: the compiled workflow digest and
// the effective goober digest.
type Pins struct {
	WorkflowDigest string
	GooberDigest   string
}

// ErrPinUnavailable means the applied generation cannot pin a start: no
// generation, a different generation, or no digests for the workflow. Callers
// fail closed and retry.
var ErrPinUnavailable = errors.New("cloud configuration: workflow digest pin is unavailable")

// PinFor returns the applied generation's digests for gaggle/workflow. The
// generation must be the applied one: a start is never pinned against a
// generation the guard no longer holds. Digests come from the candidate
// the loader assessed, so the API path never opens an archive.
func (c *Configuration) PinFor(_ context.Context, generation, gaggle, workflow string) (workflowDigest, gooberDigest string, err error) {
	gen := c.applier.Guard().current.Load()
	switch {
	case gen == nil:
		return "", "", fmt.Errorf("%w: no generation is applied", ErrPinUnavailable)
	case gen.digest != generation:
		return "", "", fmt.Errorf("%w: generation %q is not the applied generation", ErrPinUnavailable, generation)
	}
	pins, ok := gen.pins[workflowKey(gaggle, workflow)]
	if !ok || pins.WorkflowDigest == "" || pins.GooberDigest == "" {
		return "", "", fmt.Errorf("%w: no digests for %s/%s", ErrPinUnavailable, gaggle, workflow)
	}
	return pins.WorkflowDigest, pins.GooberDigest, nil
}
