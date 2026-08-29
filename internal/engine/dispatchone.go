package engine

import (
	"fmt"
	"strings"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

// DispatchOneWorkflowID composes the per-attempt identity ruling 6 requires:
// <runID>/<stage>/<attempt>. Exported as the ONE composer, so the daemon's
// runner (step 6/7) and the assertion below cannot disagree about the shape —
// a second spelling is exactly the D15 drift that would make ruling 6's
// restart safety silently untrue.
func DispatchOneWorkflowID(runID, stage string, attempt int32) string {
	return fmt.Sprintf("%s/%s/%d", runID, stage, attempt)
}

// DispatchOne executes exactly one stage attempt in a pod and returns its
// result. It is a WORKFLOW rather than a bare activity call because the
// caller is a client, not a worker: a client cannot invoke an activity, and
// the per-attempt workflow is also what makes restart safe — the daemon
// starts it with a deterministic WorkflowID (<runID>/<stage>/<attempt>) and
// REJECT_DUPLICATE, so an interrupted attempt is settled by describing that
// execution instead of by re-dispatching it (ruling 6). Double execution of
// open-pr / push-branch / merge-pr becomes impossible by construction.
//
// "By construction" is load-bearing, so it is asserted here rather than left
// to caller convention. The dedupe key Temporal enforces is the WorkflowID,
// but nothing downstream reads it: the pod name, the surrender-store key and
// the journalled attempt all come from the PAYLOAD (DispatchStage builds the
// attempt identity out of Envelope.RunID/TaskID/Attempt). A caller that
// composed the id from attempt N+1 while stamping Envelope.Attempt N would
// therefore get a fresh, non-duplicate execution re-running a mutating stage
// under an already-settled attempt's identity — the precise failure ruling 6
// exists to prevent. This workflow is the one place both are visible, and
// workflow.GetInfo's execution ID is deterministic and replay-safe, so the two
// are checked to agree and the dispatch fails closed when they do not.
//
// It deliberately carries NO goobers.run.gaggle.v1 memo, which excludes it
// from the run readers by construction rather than by an added special case —
// there is no Memo API call to remove here, and a future one would have to be
// added deliberately. Precisely:
//
//   - The projection reconciler skips any execution without that memo before
//     it does anything else (completed_runs.go), so a dispatch is never
//     projected as a run.
//   - DS6 liveness's primary arm, RunLive, describes WorkflowID == RunID; a
//     <run>/<stage>/<attempt> ID never equals a RunID, so it never matches.
//   - Its FALLBACK arm does NOT key on the memo, and the exclusion is weaker
//     there — worth stating exactly rather than over-claiming.
//     WorkflowLiveness.scanOpenWorkflows enumerates EVERY open execution and
//     inserts live[RunID(workflowID)], so open dispatches DO enter that set.
//     Harmlessly for correctness: RunID is a sha256 prefix, so a dispatch id
//     hashing onto a scheduled run's rewritten RunID is not a real risk. But
//     they DO consume the scan's budget (openWorkflowScanPageSize ×
//     maxOpenWorkflowScanPages), and overrunning it makes liveness UNKNOWN
//     (fail-live) for every scheduled-run claim in that renewal pass. One open
//     execution per in-flight stage attempt is added to a cap that until now
//     counted runs alone; if that cap ever gets close, the fix is a scoped
//     visibility query there, not a memo here.
//
// The retry budget stays with the DRIVER, not here: stageActivityOptions
// pins RetryPolicy{MaximumAttempts: 1}, so this workflow performs exactly one
// dispatch and reports its outcome. The runner's own attempt loop decides
// whether attempt N+1 happens, keeping the split policy/infrastructure budget
// arithmetic in one place (dispatchWithRetry's, mirrored by the runner) —
// classified through ClassifyDispatchFailure, which reads the same error
// shapes off this workflow's failure that it reads off an activity's.
func DispatchOne(ctx workflow.Context, in DispatchStageInput) (DispatchStageResult, error) {
	if err := refuseUnboundAttemptIdentity(workflow.GetInfo(ctx).WorkflowExecution.ID, in.Envelope); err != nil {
		return DispatchStageResult{}, err
	}
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

// refuseUnboundAttemptIdentity fails the dispatch closed unless the execution's
// WorkflowID — the key REJECT_DUPLICATE actually dedupes on — is the identity
// of the attempt in the payload, which is what every downstream reader uses.
//
// The stage is derived exactly as DispatchStage derives it, off the same two
// envelope fields, so the two cannot drift.
//
// NON-RETRYABLE and FailureTypeStage: a mismatch is a malformed request, not a
// transient fault, so retrying it would only re-refuse — and classifying it as
// infrastructure would spend the driver's infra budget on a caller bug and let
// ClassifyDispatchFailure recommend a retry that can never succeed. Policy is
// also the honest class: nothing about the substrate failed.
func refuseUnboundAttemptIdentity(workflowID string, env apiv1.InvocationEnvelope) error {
	stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
	want := DispatchOneWorkflowID(env.RunID, stage, env.Attempt)
	if workflowID == want {
		return nil
	}
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(
		"engine: DispatchOne started as %q but its payload describes attempt %q; the dedupe key and the attempt identity must be the same attempt (decision 003 ruling 6, fail closed)",
		workflowID, want), FailureTypeStage, nil)
}
