package engine

import (
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
)

const dispatchAttemptClassChange = "dispatch-attempt-class-v1"

// Preserve the activity payload of histories recorded before pod artifact
// lineage was carried across dispatch. New histories carry the retry driver's
// class; the version marker prevents replay from changing a scheduled payload.
func dispatchAttemptClass(ctx workflow.Context, class journal.AttemptClass) journal.AttemptClass {
	if workflow.GetVersion(ctx, dispatchAttemptClassChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
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
