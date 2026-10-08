package engine

import (
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

const expectedOutputsEnforcementChange = "expected-outputs-enforcement"

// enforceExpectedOutputs applies the local runner's DSL 3.1 expectedOutputs
// contract (#5175) to an engine attempt. The version marker is consulted only
// for a stage the contract covers, so a replay of a history recorded before
// enforcement keeps its original success outcome.
func enforceExpectedOutputs(ctx workflow.Context, dslVersion string, task apiv1.Task, result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	if result.Status != apiv1.ResultSuccess || !v30.EnforcesExpectedOutputs(dslVersion, task) {
		return result
	}
	if workflow.GetVersion(ctx, expectedOutputsEnforcementChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return result
	}
	return runner.EnforceExpectedOutputs(dslVersion, task, result)
}
