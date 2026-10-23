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
