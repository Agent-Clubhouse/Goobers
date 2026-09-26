package engine

import (
	"encoding/json"
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// UnmarshalJSON preserves activity metadata beside the strict revision control.
func (result *DispatchStageResult) UnmarshalJSON(data []byte) error {
	if _, err := apiv1.DecodeWorkspaceRevisionField(data); err != nil {
		return err
	}
	type plain DispatchStageResult
	// Hide ResultEnvelope's promoted method so decoding also visits the
	// flattened activity metadata, not just the embedded result.
	wire := struct {
		*plain
		UnmarshalJSON struct{} `json:"-"`
	}{plain: (*plain)(result)}
	return json.Unmarshal(data, &wire)
}

func refuseSelectedRevisionDispatch(env apiv1.InvocationEnvelope) error {
	if env.WorkspaceRevision == nil {
		return nil
	}
	return temporal.NewNonRetryableApplicationError(
		"selected-revision distributed execution is not supported yet",
		workspacerevision.CodeInvalid, nil)
}

func unsupportedRevisionResult(result apiv1.ResultEnvelope, taskType apiv1.TaskType) *workspacerevision.Error {
	return admitDistributedRevision(&result, taskType == apiv1.TaskDeterministic)
}

func admitDistributedRevision(result *apiv1.ResultEnvelope, deterministic bool) *workspacerevision.Error {
	if err := workspacerevision.NormalizeResult(result, deterministic); err != nil {
		return err
	}
	if result.WorkspaceRevision == nil {
		return nil
	}
	return &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "selected-revision distributed acceptance is not supported yet"}
}

func workspaceRevisionRejection(err error) *workspacerevision.Error {
	if revisionErr := workspacerevision.FromError(err); revisionErr != nil && revisionErr.NonRetryable() {
		return revisionErr
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case workspacerevision.CodeInvalid, workspacerevision.CodeUnauthorized, workspacerevision.CodeConflict,
			workspacerevision.CodeObjectType, workspacerevision.CodeSHAMismatch:
			return &workspacerevision.Error{Code: appErr.Type(), Message: appErr.Message(), Cause: err}
		}
	}
	return nil
}

func (r *runJournal) workspaceRevisionRefused(ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, rejection *workspacerevision.Error, identity *attemptidentity.Identity) {
	event := journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{Code: rejection.Code, Message: rejection.Error()},
	}
	addAttemptIdentity(&event, identity)
	r.append(ctx, event)
}
