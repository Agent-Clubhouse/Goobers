package engine

import (
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func gateDecisionRefusalCode(err error) (code, message string, ok bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return "", "", false
	}
	return appErr.Type(), appErr.Message(), true
}

func runHumanGateTestWorkflow(ctx workflow.Context, in RunInput) (RunResult, error) {
	return run(workflow.WithValue(ctx, humanGateImplementationKey{}, true), in)
}
