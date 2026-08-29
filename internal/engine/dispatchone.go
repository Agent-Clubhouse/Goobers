package engine

import (
	"go.temporal.io/sdk/workflow"
)

// dispatchone.go is the transport half of decision 003 ruling 2: the seam by
// which a driver that is NOT the engine's Run walk — the daemon's runner —
// places one stage attempt in a pod.
//
// The daemon must not become the pod creator (decision 011: its
// ServiceAccount keeps automountServiceAccountToken:false and zero grants,
// and allow-daemon-egress does not reach the apiserver). It already speaks
// Temporal, and goobers-worker already creates pods, so the dispatch travels
// the way the architecture §8 fabric intends: the daemon starts THIS
// workflow, the worker polls the pinned dispatch queue, and the existing
// DispatchStage activity does every bit of the Kubernetes work exactly as it
// does for an engine-driven run. One implementation of the seam, two drivers
// — never a second copy of the attempt builder (D15).
//
// Step 3 of the plan lands the contract only. Nothing constructs a client for
// it yet: the runner branch is step 6 and the daemon wiring is step 7, both
// behind GOOBERS_STAGE_DISPATCH.

// DispatchOne executes exactly one stage attempt in a pod and returns its
// result. It is a WORKFLOW rather than a bare activity call because the
// caller is a client, not a worker: a client cannot invoke an activity, and
// the per-attempt workflow is also what makes restart safe — the daemon
// starts it with a deterministic WorkflowID (<runID>/<stage>/<attempt>) and
// REJECT_DUPLICATE, so an interrupted attempt is settled by describing that
// execution instead of by re-dispatching it (ruling 6). Double execution of
// open-pr / push-branch / merge-pr becomes impossible by construction.
//
// It deliberately carries NO goobers.run.gaggle.v1 memo. Two readers key off
// that memo's presence and would both misread a dispatch as a run: the
// projection reconciler would try to project a run journal for a workflow
// that has none (completed_runs.go — a workflow with no gaggle memo is
// skipped before anything else happens), and DS6 liveness keys on
// WorkflowID == RunID, which a <run>/<stage>/<attempt> ID never satisfies.
// Both are exclusions by construction rather than by an added special case,
// which is why the memo is simply not set here — there is no Memo API call to
// remove, and a future one would have to be added deliberately.
//
// The retry budget stays with the DRIVER, not here: stageActivityOptions
// pins RetryPolicy{MaximumAttempts: 1}, so this workflow performs exactly one
// dispatch and reports its outcome. The runner's own attempt loop decides
// whether attempt N+1 happens, keeping the split policy/infrastructure budget
// arithmetic in one place (dispatchWithRetry's, mirrored by the runner) —
// classified through ClassifyDispatchFailure, which reads the same error
// shapes off this workflow's failure that it reads off an activity's.
func DispatchOne(ctx workflow.Context, in DispatchStageInput) (DispatchStageResult, error) {
	// The SAME options the engine's own remote arm applies (engine.go's
	// mode-3 branch), so a stage dispatched by the daemon and the identical
	// stage dispatched by the engine are routed and bounded identically:
	// the pinned per-(gaggle × runner-type) queue the worker polls, the
	// declared duration limit plus the worker's enforcement grace, and a
	// bounded ScheduleToStart so a queue no worker serves fails with a
	// timeout naming it instead of hanging forever.
	ctx = workflow.WithActivityOptions(ctx, stageActivityOptions(in.Envelope.Limits, in.Placement.Queue))
	var result DispatchStageResult
	if err := workflow.ExecuteActivity(ctx, ActDispatchStage, in).Get(ctx, &result); err != nil {
		// Returned bare: the caller classifies it with
		// ClassifyDispatchFailure, and wrapping would hide the
		// *temporal.ApplicationError type that classification reads.
		return DispatchStageResult{}, err
	}
	return result, nil
}
