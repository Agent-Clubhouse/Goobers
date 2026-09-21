package executor

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/invoke"
)

func (e *ShellExecutor) observeExecutionDeadline(ctx context.Context, env apiv1.InvocationEnvelope) func() {
	_, done := invoke.BeginExecution(executiondeadline.WithRecorder(ctx, e.Journal, env.TaskID, int(env.Attempt)))
	return done
}
