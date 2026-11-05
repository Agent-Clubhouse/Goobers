package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/runsemantics"
)

func classifySelfAdmissionError(err error) error {
	var refusal *runsemantics.SelfExecutionRefusal
	if errors.As(err, &refusal) {
		return temporal.NewNonRetryableApplicationError(err.Error(), runsemantics.SelfExecutionDeniedCode, err)
	}
	return classifySeamError(err)
}

// IsSelfExecutionDenied preserves the policy classification across Temporal's
// activity/workflow and serialization wrappers.
func IsSelfExecutionDenied(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var app *temporal.ApplicationError
		if errors.As(current, &app) && app.Type() == runsemantics.SelfExecutionDeniedCode {
			return true
		}
	}
	return false
}
