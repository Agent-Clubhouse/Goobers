package harness

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
)

// ChildWorkflowAccessProvider verifies a committed active stage attempt, binds
// its short-lived grant durably, and registers the secret with the shared
// scrubber before returning it. Close revokes that exact grant, even after ctx
// is cancelled. Providers must not issue access from agent-authored inputs.
type ChildWorkflowAccessProvider func(context.Context, apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error)

// WithChildWorkflowAccess configures the trusted launcher's stage-grant service.
// An envelope with child provenance fails closed when no provider is installed.
func WithChildWorkflowAccess(provider ChildWorkflowAccessProvider) Option {
	return func(e *Executor) { e.childAccess = provider }
}

func (e *Executor) prepareChildWorkflowAccess(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
	noop := func() error { return nil }
	if env.ChildWorkflowOrigin == nil {
		return nil, noop, nil
	}
	if e.childAccess == nil {
		return nil, noop, errors.New("harness: child workflow stage grant provider unavailable")
	}
	access, closeAccess, err := e.childAccess(ctx, env)
	if closeAccess == nil {
		closeAccess = noop
		if err == nil {
			err = errors.New("harness: child workflow stage grant has no revocation owner")
		}
	}
	if err == nil {
		if access == nil {
			err = errors.New("harness: child workflow stage grant unavailable")
		} else {
			err = access.Validate(env.RunID)
		}
	}
	if err != nil {
		return nil, noop, fmt.Errorf("harness: acquire child workflow stage grant: %w", errors.Join(err, closeAccess()))
	}
	copy := *access
	return &copy, closeAccess, nil
}
