package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/workspacedelta"
)

// bundle.go makes the managed mirror a CONTINUITY RECORD for mode 3 (#3803,
// delivery decision 003 ruling 5): what a pod committed arrives here as a
// workspace-delta bundle and lands on refs/heads/<run branch> under the
// per-repo lock; what a worker-side stage committed leaves here the same way.
// Worktree.Create's existing-branch arm then hands the next attempt a
// worktree on that branch exactly as it always has — the mirror was already
// how sequential self-placed stages saw each other's commits (#133); this is
// what lets pod-placed stages take part in the same record.
//
// Fast-forward-only, by construction: the reconciliation is
// workspacedelta.Reconcile, the same arms the pod applies to its own
// checkout (#3821). A diverged branch fails closed with both SHAs and is
// never resolved by force here — the mirror's run branch is shared by every
// worktree of that repo, and a forced update would clobber it for all of
// them.

// mirrorGit runs git against a managed mirror with the package's hardened
// overrides — the same invocations every other mirror operation uses.
type mirrorGit struct{}

func (mirrorGit) Run(ctx context.Context, dir string, args ...string) error {
	return runGit(ctx, dir, args...)
}

func (mirrorGit) Output(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutput(ctx, dir, args...)
}

// BundleRunBranch bundles base..<branch> from repoURL's managed mirror. It
// is the worker-side PUBLISH half: called after a self-placed stage on a
// writable repo workspace succeeds, so its commits reach the blob plane and,
// through the engine's continuity record, the next pod.
//
// Requires the mirror to exist and to hold both refs: a run branch this
// worker never created and a base it never fetched are both bugs in the
// caller's ordering, not conditions to paper over with a clone.
func (m *Manager) BundleRunBranch(ctx context.Context, repoURL, branch, base string) (workspacedelta.Bundle, error) {
	if branch == "" || base == "" {
		return workspacedelta.Bundle{}, fmt.Errorf("worktree: BundleRunBranch requires a branch and a base")
	}
	key := repoKey(repoURL)
	repoDir := m.repoDirForKey(key)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if _, err := os.Stat(repoDir); err != nil {
		return workspacedelta.Bundle{}, fmt.Errorf("worktree: bundle run branch %q: no managed working copy for %s: %w", branch, repoURL, err)
	}
	if !branchExists(ctx, repoDir, branch) {
		return workspacedelta.Bundle{}, fmt.Errorf("worktree: bundle run branch %q: branch does not exist in the working copy for %s", branch, repoURL)
	}
	b, err := workspacedelta.Create(ctx, mirrorGit{}, repoDir, "refs/heads/"+base, "refs/heads/"+branch)
	if err != nil {
		return workspacedelta.Bundle{}, fmt.Errorf("worktree: bundle run branch %q: %w", branch, err)
	}
	return b, nil
}

// ApplyBundleOptions names the receiving branch for ApplyBundle.
type ApplyBundleOptions struct {
	// RepoURL identifies the mirror; the run branch lives in it.
	RepoURL string
	// Branch is the run branch (or rebound workspace branch) the delta lands on.
	Branch string
	// BaseRef is the run's base branch name (refs/heads/<BaseRef> in the
	// mirror) — the prerequisite the bundle was cut from, and the reference
	// for the base-drift arm.
	BaseRef string
	// OwnerRunID / AcquireRemoteBranch mirror CreateOptions: when the branch
	// is a rebound one that lives on origin, it must be ACQUIRED (fetched
	// once per run) before the delta is reconciled against it — otherwise
	// this call would create it at the delta's tip and Create's own
	// acquisition would then force-reset it to origin's older head, silently
	// dropping the delta. The acquisition marker makes both calls agree.
	OwnerRunID          string
	AcquireRemoteBranch bool
}

// ApplyOutcome is what ApplyBundle did to the receiving branch.
type ApplyOutcome struct {
	Outcome workspacedelta.Outcome
	// Before is the branch's SHA before the call ("" when it did not exist);
	// After is its SHA afterwards.
	Before, After string
}

// ApplyBundle lands b on refs/heads/<opts.Branch> in repoURL's managed
// mirror, fast-forward-only (see the file comment). The mirror is refreshed
// first (WorkingCopy) so the bundle's prerequisite — the base the sender cut
// it from — is present even when this worker has not fetched since; that
// refresh is the one network operation here and is why a caller that goes on
// to Create pays for a second fetch. Diagnostics (the keep arm's two SHAs)
// go to stderr, which may be nil.
func (m *Manager) ApplyBundle(ctx context.Context, opts ApplyBundleOptions, b workspacedelta.Bundle, stderr io.Writer) (ApplyOutcome, error) {
	if opts.Branch == "" {
		return ApplyOutcome{}, fmt.Errorf("worktree: ApplyBundle requires a branch")
	}
	if opts.AcquireRemoteBranch && opts.OwnerRunID == "" {
		return ApplyOutcome{}, fmt.Errorf("worktree: ApplyBundle AcquireRemoteBranch requires OwnerRunID")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	repoDir, err := m.WorkingCopy(ctx, opts.RepoURL)
	if err != nil {
		return ApplyOutcome{}, err
	}
	key := repoKey(opts.RepoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	if opts.AcquireRemoteBranch {
		if err := m.acquireRemoteBranchLocked(ctx, key, opts.RepoURL, repoDir, opts.OwnerRunID, opts.Branch); err != nil {
			return ApplyOutcome{}, err
		}
	}
	git := mirrorGit{}
	tip, err := workspacedelta.Fetch(ctx, git, repoDir, b)
	if err != nil {
		return ApplyOutcome{}, fmt.Errorf("worktree: %w", err)
	}
	ref := "refs/heads/" + opts.Branch
	current := ""
	if branchExists(ctx, repoDir, opts.Branch) {
		current, err = gitOutput(ctx, repoDir, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return ApplyOutcome{}, fmt.Errorf("worktree: resolve run branch %q: %w", opts.Branch, err)
		}
	}
	baseRef := ""
	if opts.BaseRef != "" {
		baseRef = "refs/heads/" + opts.BaseRef
	}
	outcome, err := workspacedelta.Reconcile(ctx, git, repoDir, b.Digest, current, tip, baseRef)
	if err != nil {
		return ApplyOutcome{}, fmt.Errorf("worktree: apply workspace delta to %q: %w", opts.Branch, err)
	}
	result := ApplyOutcome{Outcome: outcome, Before: current, After: current}
	switch outcome {
	case workspacedelta.OutcomeCreate:
		// The zero old-value makes update-ref refuse if the ref appeared
		// between the existence probe and here.
		if err := runGit(ctx, repoDir, "update-ref", ref, tip, strings.Repeat("0", 40)); err != nil {
			return ApplyOutcome{}, fmt.Errorf("worktree: create run branch %q at workspace delta %s: %w", opts.Branch, b.Digest, err)
		}
		result.After = tip
	case workspacedelta.OutcomeFastForward, workspacedelta.OutcomeBaseDrift:
		if err := runGit(ctx, repoDir, "update-ref", ref, tip, current); err != nil {
			return ApplyOutcome{}, fmt.Errorf("worktree: move run branch %q onto workspace delta %s: %w", opts.Branch, b.Digest, err)
		}
		result.After = tip
		if outcome == workspacedelta.OutcomeBaseDrift {
			_, _ = fmt.Fprintf(stderr, "workspace delta %s: run branch %s at %s was an advanced base, not a diverged run; applied delta carrying %s\n", b.Digest, opts.Branch, current, tip)
		}
	case workspacedelta.OutcomeKeep:
		// The mirror already carries more than the delta does — a worker-side
		// or provider-side advance the record did not see. Say so with both
		// SHAs: the worker log is the far-side record of this decision.
		_, _ = fmt.Fprintf(stderr, "workspace delta is behind the run branch; keeping %s %s (delta %s carries %s)\n", opts.Branch, current, b.Digest, tip)
	}
	return result, nil
}

// acquireRemoteBranchLocked fetches branch from origin once per owner run,
// recording a durable acquisition marker so a retry, a later stage, or a
// process restart reuses the same logical branch without resetting commits
// made by earlier stages (CreateOptions.AcquireRemoteBranch). Callers hold
// the per-repo lock.
func (m *Manager) acquireRemoteBranchLocked(ctx context.Context, key, repoURL, repoDir, ownerRunID, branch string) error {
	acquisitionPath := m.branchAcquisitionPath(key, ownerRunID, branch)
	if _, err := os.Stat(acquisitionPath); os.IsNotExist(err) {
		ref := "refs/heads/" + branch
		if err := m.runRemoteGit(ctx, repoURL, repoDir, "fetch", "origin", "+"+ref+":"+ref); err != nil {
			return fmt.Errorf("worktree: acquire branch %q for run %s: %w", branch, ownerRunID, err)
		}
		if err := writeBranchAcquisition(acquisitionPath, branchAcquisition{
			OwnerRunID: ownerRunID,
			Branch:     branch,
		}); err != nil {
			return fmt.Errorf("worktree: record acquired branch %q for run %s: %w", branch, ownerRunID, err)
		}
	} else if err != nil {
		return fmt.Errorf("worktree: inspect acquired branch %q for run %s: %w", branch, ownerRunID, err)
	}
	return nil
}
