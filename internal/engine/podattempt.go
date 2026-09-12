package engine

import (
	"fmt"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const dispatchPodAttemptChange = "dispatch-physical-attempt-v1"

type podDispatchActivationKey struct{}
type podDispatchActivation struct{ enabled bool }

func validatePodAttempt(ordinal int) error {
	if ordinal < 0 {
		return temporal.NewNonRetryableApplicationError("engine: podAttempt must be zero (legacy) or positive", FailureTypeStage, nil)
	}
	return nil
}

func dispatchPodAttemptChangeID(stage string, ordinal int) string {
	return fmt.Sprintf("%s/%s/%d", dispatchPodAttemptChange, stage, ordinal)
}

// Each historical dispatch retains its recorded payload. Versioning each
// physical dispatch also lets an upgraded in-flight workflow use fresh keys
// when it schedules future pods, instead of pinning unsafe legacy reentry for
// the rest of that execution. The counter includes replayed dispatches.
// After activation, deterministic workflow-local state avoids further version
// markers and accumulated TemporalChangeVersion visibility growth.
func dispatchPodAttempt(ctx workflow.Context, stage string, ordinal int) int {
	state, _ := ctx.Value(podDispatchActivationKey{}).(*podDispatchActivation)
	if state != nil && state.enabled {
		return ordinal
	}
	if workflow.GetVersion(ctx, dispatchPodAttemptChangeID(stage, ordinal), workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return 0
	}
	if state != nil {
		state.enabled = true
	}
	return ordinal
}
