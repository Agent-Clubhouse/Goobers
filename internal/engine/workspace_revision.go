package engine

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func refuseSelectedRevisionDispatch(env apiv1.InvocationEnvelope) error {
	if env.WorkspaceRevision == nil {
		return nil
	}
	return temporal.NewNonRetryableApplicationError(
		"selected-revision distributed execution is not supported yet",
		workspacerevision.CodeInvalid, nil)
}

func unsupportedRevisionResult(result apiv1.ResultEnvelope, taskType apiv1.TaskType) *workspacerevision.Error {
	if result.WorkspaceRevision == nil {
		return nil
	}
	if taskType != apiv1.TaskDeterministic {
		return &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "agentic results cannot establish workspace revision authority"}
	}
	if result.Status != apiv1.ResultSuccess {
		return nil
	}
	return &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "selected-revision distributed acceptance is not supported yet"}
}

func (r *runJournal) workspaceRevisionRefused(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, rejection *workspacerevision.Error, identity *attemptidentity.Identity) {
	event := journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{Code: rejection.Code, Message: rejection.Error()},
	}
	addAttemptIdentity(&event, identity)
	r.append(ctx, event)
}
