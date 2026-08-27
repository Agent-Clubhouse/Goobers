package main

// Cross-stage workspace continuity for mode 3 (#3763).
//
// THE PROBLEM. On the worker, WorktreeWorkspaces.Provision hands each attempt a
// fresh worktree on the SAME run branch in the SAME mirror clone: the branch ref
// and the object store are shared, so a commit made by one stage is simply there
// for the next. Continuity is the local branch ref, and nothing has to carry it.
//
// Mode 3 has no shared filesystem. Each attempt gets a fresh pod with a fresh
// clone and the pod is disposed after surrender (D1/D3 — reuse would be a
// correctness bug), so a commit made in one pod dies with it. MEASURED (run
// e1cfcfe2): the pod arm committed e58232a and the next stage reported
// HEAD=21d228e, the PRE-COMMIT head, and continued from base reporting success.
// The self arm of the same run carried its commit forward. Every mutating
// workflow in the product commits in one stage and pushes in a later one, so in
// mode 3 they all ship nothing and say they succeeded.
//
// THE CARRIER. The stage's commits travel as a git bundle through the blob
// plane — the same claim-check shape the agentic kit already uses for oversized
// activity arguments, and for the same reason: the payload is too big for the
// control plane, and content addressing makes substitution detectable rather
// than merely unlikely. The engine threads only the digest, exactly as it
// already threads workspaceBranch across stages.
//
// WHY NOT PUSH TO THE RUN BRANCH. It is simpler and it changes observable
// semantics: commits would become visible on the remote before the gates that
// guard them run, open-pr could find the branch already present, and
// pr-remediation's force-push-with-lease would have its lease baseline moved.
// The run branch stays written by push-branch alone.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
)

// workspaceDeltaRef is the ref name the bundle carries its commits under. A
// fixed private name rather than the run branch: the receiving side fetches by
// this name and never has to agree with the sender about branch naming, and it
// cannot collide with a real branch in the bundle's namespace.
const workspaceDeltaRef = "refs/goobers/workspace-delta"

// podBlobClient builds the pod's blob-plane client, or nil when this pod has no
// blob endpoint (the pre-continuity deployment shape).
func podBlobClient() *dispatcher.BlobClient {
	endpoint := strings.TrimSpace(os.Getenv(dispatcher.EnvBlobEndpoint))
	if endpoint == "" {
		return nil
	}
	return &dispatcher.BlobClient{BaseURL: endpoint, Token: os.Getenv(dispatcher.EnvPodToken)}
}

// stageWorkspaceIsWritableRepo reports whether this stage's declared workspace
// is one whose commits are worth carrying. scratch has no repo; repo-readonly is
// detached at base and must not produce a delta at all — a read-only stage that
// somehow committed would otherwise silently rewrite what later stages see.
func stageWorkspaceIsWritableRepo() bool {
	mode := apiv1.WorkspaceMode(strings.TrimSpace(os.Getenv(dispatcher.EnvStageWorkspace)))
	return mode.IsWritableRepo()
}

// publishWorkspaceDelta bundles whatever this stage committed beyond the run's
// base and publishes it to the blob plane, returning the digest the next stage
// needs. It returns "" when there is nothing to carry — not a repo workspace, or
// a repo workspace with no new commits — both of which are ordinary.
//
// A failure here is NOT ordinary and is returned as an error: the commits exist,
// nothing else will carry them, and reporting success would strand exactly the
// diff this exists to preserve.
func publishWorkspaceDelta(ctx context.Context, dir string, stderr io.Writer) (string, error) {
	if !stageWorkspaceIsWritableRepo() {
		return "", nil
	}
	branch, err := currentBranch(dir)
	if err != nil {
		// Detached HEAD in a writable repo workspace: nothing sensible to
		// bundle, and this is not the place to diagnose it.
		pf(stderr, "workspace delta: no checked-out branch (%v); nothing to carry\n", err)
		return "", nil
	}
	empty, err := branchHasNoCommitsBeyondBase(dir, branch)
	if err != nil {
		// Cannot prove there is nothing to carry, so do not claim there isn't.
		// Falling through to bundle is the safe direction: an empty bundle is
		// wasteful, a skipped one loses work.
		pf(stderr, "workspace delta: could not count commits on %q (%v); bundling anyway\n", branch, err)
	} else if empty {
		return "", nil
	}

	client := podBlobClient()
	if client == nil {
		return "", fmt.Errorf("stage committed to %s but this pod has no %s; the commits cannot reach the next stage", branch, dispatcher.EnvBlobEndpoint)
	}
	baseRef, err := resolveBaseRef(dir, stageBaseBranch())
	if err != nil {
		return "", fmt.Errorf("workspace delta: %w", err)
	}

	// The bundle is written OUTSIDE the workspace: the workspace is the repo,
	// and a stray file in it would show up as an untracked change to whatever
	// runs next (and to `git status` in the agent's own view).
	tmp, err := os.MkdirTemp("", "goobers-delta-")
	if err != nil {
		return "", fmt.Errorf("workspace delta: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bundlePath := filepath.Join(tmp, "workspace.bundle")

	// A THIN bundle (base..branch) carries only this run's commits and needs the
	// base object present on the receiving side, which it is: the next pod
	// clones the same base. If the base has advanced under us, git refuses the
	// fetch with "does not contain prerequisite commits" — loud and named,
	// which is the right failure direction for a carrier of real work.
	// The bundle must NAME the ref the receiving side fetches. `git bundle
	// create <f> <base>..<branch>` records refs/heads/<branch>, which would
	// force both sides to agree on branch naming; pointing a fixed private ref
	// at HEAD first makes the carrier self-describing instead.
	if err := runGit(ctx, dir, nil, stderr, "update-ref", workspaceDeltaRef, "HEAD"); err != nil {
		return "", fmt.Errorf("workspace delta: name delta ref: %w", err)
	}
	defer func() { _ = runGit(ctx, dir, nil, io.Discard, "update-ref", "-d", workspaceDeltaRef) }()

	if err := runGit(ctx, dir, nil, stderr, "bundle", "create", bundlePath,
		baseRef+".."+workspaceDeltaRef); err != nil {
		return "", fmt.Errorf("workspace delta: bundle %s..%s: %w", baseRef, branch, err)
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return "", fmt.Errorf("workspace delta: read bundle: %w", err)
	}
	digest := agentickit.Digest(data)
	if err := client.Put(ctx, digest, data); err != nil {
		return "", fmt.Errorf("workspace delta: publish %s (%d bytes): %w", digest, len(data), err)
	}
	pf(stderr, "workspace delta: published %s (%d bytes) carrying %s..%s\n", digest, len(data), baseRef, branch)
	return digest, nil
}

// applyWorkspaceDelta fetches the previous stage's bundle and moves the
// checked-out branch onto it, so this stage continues from what the last one
// committed rather than from base.
//
// Every failure here is fatal to the stage. Continuing without the delta is the
// silent-wrong-result this whole mechanism exists to prevent: the stage would
// run against base, quite possibly succeed, and ship a diff that silently
// dropped its predecessor's work.
func applyWorkspaceDelta(ctx context.Context, dir, digest string, gitEnv []string, stderr io.Writer) error {
	client := podBlobClient()
	if client == nil {
		return fmt.Errorf("stage was handed workspace delta %s but this pod has no %s", digest, dispatcher.EnvBlobEndpoint)
	}
	data, err := client.Get(ctx, digest)
	if err != nil {
		return fmt.Errorf("fetch workspace delta %s: %w", digest, err)
	}
	// Verify the content address before handing the bytes to git. The kit does
	// the same on arrival and for the same reason: a substituted delta means
	// running this stage on top of commits nobody in this run made.
	if got := agentickit.Digest(data); got != digest {
		return fmt.Errorf("workspace delta digest mismatch: got %s, expected %s", got, digest)
	}
	tmp, err := os.MkdirTemp("", "goobers-delta-")
	if err != nil {
		return fmt.Errorf("workspace delta: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bundlePath := filepath.Join(tmp, "workspace.bundle")
	if err := os.WriteFile(bundlePath, data, 0o600); err != nil {
		return fmt.Errorf("workspace delta: write bundle: %w", err)
	}

	if err := runGit(ctx, dir, gitEnv, stderr, "fetch", "--quiet", bundlePath, workspaceDeltaRef); err != nil {
		return fmt.Errorf("apply workspace delta %s: %w", digest, err)
	}
	if err := runGit(ctx, dir, gitEnv, stderr, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("move onto workspace delta %s: %w", digest, err)
	}
	return nil
}
