package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/runner"
)

func classifySelfAdmissionError(err error) error {
	var refusal *runner.SelfExecutionRefusal
	if errors.As(err, &refusal) {
		return temporal.NewNonRetryableApplicationError(err.Error(), runner.SelfExecutionDeniedCode, err)
	}
	return classifySeamError(err)
}
