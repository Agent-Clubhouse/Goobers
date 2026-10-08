package engine

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// probeRunBranchDiff reads the run's committed diff for an implementation-
// review gate whose reviewer declared a workspace that is not on the run
// branch (#5414). A repo-readonly reviewer is detached at base and a scratch
// reviewer has no checkout at all, so the reviewer's own workspace can only
// ever report "cannot tell" — and the reviewer was then asked to review a
// change it was never given, reaching for git and judging the base branch.
//
// The probe is a short-lived writable workspace on the run branch, landed
// from the same continuity delta a writable reviewer would have been handed,
// read exactly as a writable reviewer's own workspace is read
// (captureGateDiff), and torn down BEFORE the reviewer's own workspace is
// provisioned: both are keyed by this gate's stage, so the two must never
// coexist. The reviewer never sees the probe's path — only the diff it
// yields, attached as the usual "<gate>.diff" pointer — so its declared
// workspace semantics are unchanged.
//
// Returns nil when no probe applies: the gate does not require a diff, or
// the reviewer is already on the run branch and reads its own workspace.
func (a *Activities) probeRunBranchDiff(ctx context.Context, env apiv1.InvocationEnvelope, workspace apiv1.WorkspaceMode, workspaceBranch, workspaceDelta string, requireDiff bool) (*GateReviewResult, error) {
	if !requireDiff || writableWorkspace(workspace) {
		return nil, nil
	}
	probeEnv := env
	ws, err := a.provisionWorkspace(ctx, &probeEnv, apiv1.WorkspaceRepo, false, workspaceBranch, workspaceDelta)
	if err != nil {
		return nil, err
	}
	defer removeWorkspace(ctx, env.TaskID, ws)
	out := captureGateDiff(ctx, ws, apiv1.WorkspaceRepo, probeEnv)
	return &out, nil
}
