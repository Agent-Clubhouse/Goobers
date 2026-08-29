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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
		// NOT "nothing to carry". This stage declared a writable repo
		// workspace, so it may well have committed; being unable to name the
		// branch means we cannot tell, and answering "" would silently drop
		// whatever it did. That is the exact failure shape this mechanism
		// exists to remove, and writing it as an ordinary case here cost five
		// deploy cycles to find: in-pod every git call failed on dubious
		// ownership, and this swallowed it as a benign skip.
		return "", fmt.Errorf("workspace delta: cannot determine the checked-out branch of the writable repo workspace: %w", err)
	}
	empty, err := branchHasNoCommitsBeyondBase(dir, branch)
	if err != nil {
		// Still not fatal — bundling an empty delta is wasteful, skipping a
		// real one loses work, so the uncertain direction is to bundle.
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
//
// THE ANCESTRY GUARD. The digest this stage was handed is whatever the engine
// last recorded, which today is last-writer, not "the producer this stage
// actually depends on" (#3767) — and self-placed (worker) stages and
// provider-side producers (update-behind-pr) advance the run branch without
// ever publishing a delta at all. Either gap can hand a pod a STALE digest
// while its own checkout is already ahead of it — self-placed stages never
// publish, so a pod that follows one still carries whatever the last POD
// published. Blindly `reset --hard`ing onto that digest would silently
// rewind real work back to a prior commit. So before moving anything, compare
// the checkout's current HEAD against the fetched tip: HEAD already contains
// the delta (fast-forward or equal) -> apply as before; the delta is strictly
// behind HEAD -> the checkout already has more than the delta carries, so keep
// it and say so; neither contains the other -> the two histories genuinely
// disagree and this must fail rather than guess.
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

	// Resolve both tips before touching anything. Neither call needs the
	// stage's credential (this is all local, post-fetch); routed through
	// workspaceGitCommand for the same safe.directory reason every other
	// delta-path git call is (see composeGitEnv's comment) — an in-pod
	// workspace fails "detected dubious ownership" on any call that skips it.
	head, err := workspaceRevParse(dir, "HEAD")
	if err != nil {
		return fmt.Errorf("workspace delta %s: determine current HEAD: %w", digest, err)
	}
	tip, err := workspaceRevParse(dir, "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("workspace delta %s: determine fetched tip: %w", digest, err)
	}

	// HEAD already contains the delta's tip (equal counts, since a commit is
	// its own ancestor) -> this is an ordinary fast-forward, apply it.
	fastForward, err := workspaceIsAncestor(dir, head, tip)
	if err != nil {
		return fmt.Errorf("workspace delta %s: check whether %s is an ancestor of %s: %w", digest, head, tip, err)
	}
	if fastForward {
		if err := runGit(ctx, dir, gitEnv, stderr, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
			return fmt.Errorf("move onto workspace delta %s: %w", digest, err)
		}
		return nil
	}

	// The delta's tip is strictly behind HEAD -> the checkout already carries
	// everything the delta does and then some (a self-placed stage or a
	// provider-side producer such as update-behind-pr advanced the branch
	// after this digest was published). Resetting onto it would rewind that
	// work. Keep what is checked out and say so, loudly enough that the
	// stage's own stderr — the only record that survives pod disposal —
	// carries the far-side evidence.
	behind, err := workspaceIsAncestor(dir, tip, head)
	if err != nil {
		return fmt.Errorf("workspace delta %s: check whether %s is an ancestor of %s: %w", digest, tip, head, err)
	}
	if behind {
		pf(stderr, "workspace delta is behind the checkout; keeping %s (delta %s carries %s)\n", head, digest, tip)
		return nil
	}

	// Neither is an ancestor of the other: the checkout and the delta each
	// carry commits the other lacks. There is no safe automatic resolution —
	// a merge or rebase here would be inventing history nobody in this run
	// asked for — so fail closed and name both tips.
	return fmt.Errorf("workspace delta %s has diverged from the checked-out branch: checkout is at %s, delta carries %s (neither is an ancestor of the other); refusing to overwrite local history", digest, head, tip)
}

// workspaceRevParse resolves ref to the commit SHA it names within dir.
// Routed through workspaceGitCommand for the safe.directory exemption every
// delta-path git call needs in-pod.
func workspaceRevParse(dir, ref string) (string, error) {
	out, err := workspaceGitCommand(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// workspaceIsAncestor reports whether commit ancestor is reachable from
// descendant — a commit counts as its own ancestor, matching git's own
// `merge-base --is-ancestor`. Routed through workspaceGitCommand rather than
// the package's other gitIsAncestor (remediationcollision.go), which runs
// with the bare process environment: that is fine on the worker, where it is
// used today, but an in-pod workspace needs the safe.directory exemption on
// every call or "detected dubious ownership" resurfaces exactly as #3763's
// own history describes above.
func workspaceIsAncestor(dir, ancestor, descendant string) (bool, error) {
	err := workspaceGitCommand(dir, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("merge-base --is-ancestor %s %s: %w", ancestor, descendant, err)
}
