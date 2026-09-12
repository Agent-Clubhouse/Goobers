package engine

import (
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/journal"
)

// terminalWorkflowFailure preserves an exhausted infrastructure budget across
// the final workflow failure boundary. The SDK serializes fmt wrappers as new
// application types, hiding the inner infrastructure marker from consumers.
// Classify before serialization, retaining the exhaustion message and cause.
// An explicit policy application error is never promoted from its inner cause.
func terminalWorkflowFailure(err error) error {
	if class, classifyErr := ClassifyDispatchFailure(err); classifyErr == nil && class == journal.AttemptInfra {
		return temporal.NewApplicationErrorWithCause(err.Error(), FailureTypeInfrastructure, err)
	}
	return err
}
