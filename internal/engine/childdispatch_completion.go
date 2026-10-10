package engine

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
)

// ChildDispatchCompletion is the existing worker's authenticated Temporal
// completion operation. Its task token binds the report to the exact activity;
// it cannot start a workflow, create a pod or grant a replacement attempt.
type ChildDispatchCompletion interface {
	CompleteActivityWithOptions(context.Context, client.CompleteActivityOptions) error
}

func (a *Activities) completeChildDispatch(ctx context.Context, out ChildDispatchResult) (ChildDispatchResult, error) {
	if a.ChildDispatchCompletion == nil || !activity.IsActivity(ctx) || ctx.Err() == nil {
		return out, nil
	}
	select {
	case <-activity.GetWorkerStopChannel(ctx):
	default:
		return out, nil
	}
	// Worker shutdown cancels the SDK's activity background context. Its
	// outbound payload processing then fails even after dispatch has finished
	// bounded cleanup. Complete explicitly using that same task token and an
	// independent bounded context; only a confirmed completion suppresses the
	// ordinary response. Missing custody stays missing in the recorded report.
	info := activity.GetInfo(ctx)
	if info.WorkflowType == nil || info.WorkflowExecution.ID == "" || len(info.TaskToken) == 0 {
		return ChildDispatchResult{}, errors.New("child custody completion requires a workflow-bound task")
	}
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := a.ChildDispatchCompletion.CompleteActivityWithOptions(completionCtx, client.CompleteActivityOptions{
		TaskToken: info.TaskToken, Result: out, Namespace: info.Namespace,
		WorkflowID: info.WorkflowExecution.ID, WorkflowType: info.WorkflowType.Name,
		ActivityType: info.ActivityType.Name, TaskQueue: info.TaskQueue,
	})
	if err != nil {
		return ChildDispatchResult{}, errors.Join(errors.New("commit stopped worker child custody"), err)
	}
	return ChildDispatchResult{}, activity.ErrResultPending
}
