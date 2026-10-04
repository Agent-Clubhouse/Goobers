package engine

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

const declaredArtifactRetryChange = "declared-artifact-retry-v1"

// declaredArtifactRetryError is the engine arm of the runner's #5560 rule: a
// clean agentic dispatch whose result is a retryable declared-artifact failure
// becomes a policy-class stage failure while a policy attempt remains, so the
// retry loop spends retry.maxAttempts on it. The final attempt keeps its
// ordinary failure result (stage.finished, continueOnError, failure routing).
//
// The version marker is taken only on the branch that changes behavior, so a
// history recorded before this rule replays its artifact failure as the
// stage.finished it originally journaled rather than scheduling a retry.
func declaredArtifactRetryError(ctx workflow.Context, taskType apiv1.TaskType, res apiv1.ResultEnvelope, policyRetryRemains bool) error {
	if taskType != apiv1.TaskAgentic || !policyRetryRemains {
		return nil
	}
	retryErr := runner.DeclaredArtifactRetryFailure(res)
	if retryErr == nil {
		return nil
	}
	if workflow.GetVersion(ctx, declaredArtifactRetryChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return nil
	}
	return temporal.NewApplicationError(retryErr.Error(), FailureTypeStage)
}
