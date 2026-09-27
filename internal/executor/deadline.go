package executor

import (
	"context"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
	"github.com/goobers/goobers/internal/invoke"
)

func (e *ShellExecutor) observeExecutionDeadline(ctx context.Context, env apiv1.InvocationEnvelope) func() {
	_, done := invoke.BeginExecution(executiondeadline.WithRecorder(ctx, e.Journal, strings.TrimPrefix(env.TaskID, env.RunID+":"), int(env.Attempt)))
	return done
}
