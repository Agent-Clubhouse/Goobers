package runtimewiring

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

// DeliveredChildWorkflowAccess serves the child-workflow grant control bound
// for an engine parent's stage attempt, which the dispatcher delivers in
// dispatcher.EnvChildWorkflowAccess (docs/design/engine-parent-child-authority.md
// §2 step 4). A pod without it defers to fallback, the contained-parent
// exchange. The grant goes only to the child-workflow tool's private config
// and the registry; the engine worker releases it when the stage activity
// ends, so closing it here does nothing.
func DeliveredChildWorkflowAccess(registry *journal.RegistryScrubber, fallback harness.ChildWorkflowAccessProvider) harness.ChildWorkflowAccessProvider {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		delivered, ok := os.LookupEnv(dispatcher.EnvChildWorkflowAccess)
		if !ok {
			if fallback == nil {
				return nil, nil, errors.New("child workflow access unavailable")
			}
			return fallback(ctx, env)
		}
		if env.ChildWorkflowOrigin == nil || registry == nil {
			return nil, nil, errors.New("delivered child workflow access has no origin or secret registry")
		}
		var access mcpio.ChildWorkflowAccess
		if err := json.Unmarshal([]byte(delivered), &access); err != nil {
			// Never echo the value: it is a bearer.
			return nil, nil, errors.New("delivered child workflow access is malformed")
		}
		if err := access.Validate(env.RunID); err != nil {
			return nil, nil, err
		}
		registry.Register([]byte(access.BearerToken))
		return &access, func() error { return nil }, nil
	}
}
