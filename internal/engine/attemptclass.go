package engine

import (
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

const dispatchAttemptClassChange = "dispatch-attempt-class-v1"

// Preserve historical nil-binding payloads and their workflow-wide marker.
// A newly versioned per-start binding instead carries the durable start's class,
// including a retry scheduled after a legacy workflow resumes on new code.
func dispatchAttemptClass(ctx workflow.Context, class journal.AttemptClass, binding *launchreceipt.Binding) journal.AttemptClass {
	version := workflow.GetVersion(ctx, dispatchAttemptClassChange, workflow.DefaultVersion, 1)
	if binding != nil {
		return binding.Class
	}
	if version == workflow.DefaultVersion {
		return ""
	}
	return class
}

// Gate policy accounting, unlike the pod counter, excludes evaluator failures.
func gateInitialAttemptClass(policyAttempts, infrastructureAttempts int) journal.AttemptClass {
	if infrastructureAttempts > 0 {
		return journal.AttemptInfra
	}
	if policyAttempts > 0 {
		return journal.AttemptPolicy
	}
	return ""
}
