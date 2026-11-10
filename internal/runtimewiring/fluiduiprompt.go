package runtimewiring

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/harness"
)

// FluidUIPromptSource hands a Labs Fluid UI run's agent the request text as a
// private input, so the prompt never enters the run's artifacts or transcript
// (#8425). Runs of every other workflow get nothing; a fluid-ui-customize run
// with no bound request fails closed rather than draft without one.
func FluidUIPromptSource(instanceRoot string) harness.PrivateInputSource {
	requests := fluidui.NewRequests(fluidui.ForInstance(instanceRoot), nil)
	return func(_ context.Context, env apiv1.InvocationEnvelope) ([]harness.PrivateInput, error) {
		if env.WorkflowID != fluidui.Workflow {
			return nil, nil
		}
		prompt, err := requests.PromptForRun(env.RunID)
		if errors.Is(err, fluidui.ErrBindingNotFound) {
			return nil, fmt.Errorf("fluid UI run %s has no bound request", env.RunID)
		}
		if err != nil {
			return nil, err
		}
		return []harness.PrivateInput{{
			Name:      fluidui.PrivatePromptFile,
			Data:      []byte(prompt),
			Sensitive: fluidui.SensitivePromptValues(prompt),
		}}, nil
	}
}
