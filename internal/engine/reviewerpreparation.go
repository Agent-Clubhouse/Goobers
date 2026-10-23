package engine

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
)

const (
	reviewerPreparationChange = "reviewer-diff-before-dispatch-v1"
	ActPrepareReviewerDiff    = "PrepareReviewerDiff"
)

// ReviewerPreparation contains only scrubbed evidence plus source digests and
// sizes. The activity never journals: keyed workflow emission owns that commit.
type ReviewerPreparation struct {
	Diff      []byte                             `json:"diff,omitempty"`
	Observed  bool                               `json:"observed"`
	Redaction runsemantics.ReviewerDiffRedaction `json:"redaction"`
}

// PrepareReviewerDiff observes the selected continuity state without executing
// a stage. Repeating it creates no journal events or reviewer invocations.
func (a *Activities) PrepareReviewerDiff(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch, workspaceDelta string, mode apiv1.WorkspaceMode, requireDiff bool) (ReviewerPreparation, error) {
	if err := a.refuseLeakedEnvelope(env); err != nil {
		return ReviewerPreparation{}, err
	}
	if mode == "" {
		mode = apiv1.WorkspaceRepo
	}
	if !requireDiff && !writableWorkspace(mode) {
		return ReviewerPreparation{}, nil // No run-branch observation applies.
	}
	out, err := a.probeRunBranchDiff(ctx, env, mode, workspaceBranch, workspaceDelta, requireDiff)
	if err != nil {
		return ReviewerPreparation{}, classifySeamError(err)
	}
	if out == nil {
		ws, err := a.provisionWorkspace(ctx, &env, mode, false, workspaceBranch, workspaceDelta)
		if err != nil {
			return ReviewerPreparation{}, classifySeamError(err)
		}
		capture := captureGateDiff(ctx, ws, mode, env)
		removeWorkspace(ctx, env.TaskID, ws)
		out = &capture
	}
	source, scrubbed := out.Diff, out.Diff
	if a.Canary != nil {
		scrubbed = a.Canary.Scrub(source)
	}
	// Match local recordReviewerDiff: registry scrub first, then the recorder's
	// boundary scrubber. The annotation describes the pre-artifact scrub step.
	redaction := runsemantics.ObserveReviewerDiffRedaction(source, scrubbed)
	return ReviewerPreparation{Diff: a.scrubber().Scrub(scrubbed), Observed: out.Observed, Redaction: redaction}, nil
}

func (r *runJournal) prepareReviewerDiff(ctx workflow.Context, env *apiv1.InvocationEnvelope, workspaceBranch, workspaceDelta string, mode apiv1.WorkspaceMode, requireDiff bool) (*apiv1.ContextPointer, error) {
	if workflow.GetVersion(ctx, reviewerPreparationChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return nil, nil
	}
	var prepared ReviewerPreparation
	options := workflow.GetActivityOptions(ctx)
	options.HeartbeatTimeout = 0 // Preparation does not dispatch or monitor a pod.
	prepareCtx := workflow.WithActivityOptions(ctx, options)
	if err := workflow.ExecuteActivity(prepareCtx, ActPrepareReviewerDiff, *env, workspaceBranch, workspaceDelta, mode, requireDiff).Get(ctx, &prepared); err != nil {
		return nil, err
	}
	gate := gateNameFromTaskID(*env)
	pointer, err := r.recordReviewerDiff(ctx, env.RunID, gate, prepared.Diff)
	if err != nil {
		return nil, err
	}
	env.PreparedReviewerDiff = &apiv1.PreparedReviewerDiff{Observed: prepared.Observed}
	if pointer != nil {
		env.PreparedReviewerDiff.Artifact = pointer.Artifact
		// The system-authored pointer supersedes any earlier same-name evidence.
		pointers := make([]apiv1.ContextPointer, 0, len(env.ContextPointers)+1)
		for _, existing := range env.ContextPointers {
			if existing.Name != pointer.Name {
				pointers = append(pointers, existing)
			}
		}
		env.ContextPointers = append(pointers, *pointer)
		ref := journalRefFrom(*pointer.Artifact)
		r.append(ctx, prepared.Redaction.Event(gate, ref))
	}
	// This must commit before gate.started / reviewer.started. The journal owner
	// assigns stable keys to both operations, including lost-response retries.
	if err := r.emitPending(ctx); err != nil {
		return nil, err
	}
	return pointer, nil
}

func (r *runJournal) reviewerDiffPointer(ctx workflow.Context, runID, gate string, review GateReviewResult) (*apiv1.ContextPointer, error) {
	if review.Prepared {
		return review.PreparedDiff, nil
	}
	return r.recordReviewerDiff(ctx, runID, gate, review.Diff)
}

func (a *Activities) reviewerDiffForInvocation(ctx context.Context, env *apiv1.InvocationEnvelope, ws Workspace, mode apiv1.WorkspaceMode, probed *GateReviewResult) (GateReviewResult, error) {
	if env.PreparedReviewerDiff != nil {
		pointer, err := runsemantics.PreparedReviewerDiffPointer(*env)
		if err != nil {
			return GateReviewResult{}, classifySeamError(err)
		}
		out := GateReviewResult{Prepared: true, PreparedDiff: pointer, Observed: env.PreparedReviewerDiff.Observed}
		if pointer != nil {
			out.DiffDigest = pointer.Artifact.Digest
		}
		return out, nil
	}
	out := captureGateDiff(ctx, ws, mode, *env)
	if probed != nil {
		out = *probed
	}
	if a.Canary != nil && len(out.Diff) > 0 {
		out.Diff = a.Canary.Scrub(out.Diff)
	}
	if len(out.Diff) > 0 {
		ref, err := journal.ArtifactRef(out.Diff)
		if err != nil {
			return GateReviewResult{}, classifySeamError(fmt.Errorf("address reviewer diff for gate %q: %w", env.TaskID, err))
		}
		out.DiffDigest = ref.Digest
		env.ContextPointers = append(env.ContextPointers, apiv1.ContextPointer{
			Name: runsemantics.ReviewerDiffPointerName(gateNameFromTaskID(*env)), Integrity: apiv1.IntegrityDerived,
			Artifact: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: runsemantics.ReviewerDiffMediaType, Integrity: apiv1.IntegrityDerived},
		})
	}
	return out, nil
}

func (r *runJournal) reviewerEvidencePointers(ctx workflow.Context, runID, gate string, review GateReviewResult, pointers []apiv1.ContextPointer) ([]apiv1.ContextPointer, error) {
	pointer, err := r.reviewerDiffPointer(ctx, runID, gate, review)
	if err != nil {
		return nil, err
	}
	if pointer != nil {
		pointers = append(pointers, *pointer)
	}
	return pointers, nil
}
