package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/runner"
)

// IsSelfExecutionDenied preserves the policy classification across Temporal's
// activity/workflow and serialization wrappers.
func IsSelfExecutionDenied(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var app *temporal.ApplicationError
		if errors.As(current, &app) && app.Type() == runner.SelfExecutionDeniedCode {
			return true
		}
	}
	return false
}
