package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workspacedelta"
)

// bundleFixture is one origin plus a manager whose mirror of it is warm.
type bundleFixture struct {
	origin  string
	manager *Manager
}

func newBundleFixture(t *testing.T) bundleFixture {
	t.Helper()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	if _, err := m.WorkingCopy(context.Background(), repo); err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	return bundleFixture{origin: repo, manager: m}
}

// publishFrom clones the origin, commits file on branch and returns the
// bundle base..branch as a pod would publish it, plus the commit SHA.
func (f bundleFixture) publishFrom(t *testing.T, branch, file string) (workspacedelta.Bundle, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pod")
	runTestGit(t, filepath.Dir(dir), "clone", "-q", "--branch", "main", f.origin, dir)
	runTestGit(t, dir, "config", "user.name", "pod")
	runTestGit(t, dir, "config", "user.email", "pod@example.com")
	runTestGit(t, dir, "checkout", "-q", "-B", branch)
	mustWriteFile(t, filepath.Join(dir, file), file+"\n")
	runTestGit(t, dir, "add", file)
	runTestGit(t, dir, "commit", "-q", "-m", "commit "+file)
	head := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	b, err := workspacedelta.Create(context.Background(), mirrorGit{}, dir, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("Create bundle: %v", err)
	}
	return b, head
}

func (f bundleFixture) branchSHA(t *testing.T, branch string) string {
	t.Helper()
	out, err := gitOutput(context.Background(), f.manager.repoDirForKey(repoKey(f.origin)), "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		t.Fatalf("rev-parse %s in mirror: %v", branch, err)
	}
	return out
}

// ApplyBundle's four arms on the mirror, and BundleRunBranch cutting the
// same content back out: absent -> create, ff -> update-ref, ahead -> keep
// with both SHAs on stderr, diverged -> named error and the branch untouched.
func TestApplyBundleArmsAndBundleRunBranch(t *testing.T) {
	ctx := context.Background()
	const branch = "goobers/wf/run-apply"
	f := newBundleFixture(t)
	opts := ApplyBundleOptions{RepoURL: f.origin, Branch: branch, BaseRef: "main"}

	// absent -> create
	first, firstHead := f.publishFrom(t, branch, "first.txt")
	var stderr strings.Builder
	out, err := f.manager.ApplyBundle(ctx, opts, first, &stderr)
	if err != nil {
		t.Fatalf("ApplyBundle (absent): %v", err)
	}
	if out.Outcome != workspacedelta.OutcomeCreate || out.Before != "" || out.After != firstHead {
		t.Fatalf("ApplyBundle (absent) = %+v, want create at %s", out, firstHead)
	}
	if got := f.branchSHA(t, branch); got != firstHead {
		t.Fatalf("mirror %s = %s, want the bundle tip %s", branch, got, firstHead)
	}

	// The worktree the next self stage gets is on that branch at the tip —
	// the whole point (#3803).
	wt, err := f.manager.Create(ctx, CreateOptions{RepoURL: f.origin, RunID: "run-apply-consume", BaseRef: "main", Branch: branch})
	if err != nil {
		t.Fatalf("Create worktree: %v", err)
	}
	if head, _ := wt.HeadSHA(ctx); head != firstHead {
		t.Fatalf("worktree HEAD = %s, want %s", head, firstHead)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "first.txt")); err != nil {
		t.Fatalf("the pod's commit did not reach the worktree: %v", err)
	}
	// A worker-side stage commits on top, and BundleRunBranch publishes it.
	runTestGit(t, wt.Path, "config", "user.name", "worker")
	runTestGit(t, wt.Path, "config", "user.email", "worker@example.com")
	mustWriteFile(t, filepath.Join(wt.Path, "second.txt"), "second\n")
	runTestGit(t, wt.Path, "add", "second.txt")
	runTestGit(t, wt.Path, "commit", "-q", "-m", "second")
	secondHead := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	published, err := f.manager.BundleRunBranch(ctx, f.origin, branch, "main")
	if err != nil {
		t.Fatalf("BundleRunBranch: %v", err)
	}
	if published.Tip != secondHead || published.Digest != workspacedelta.Digest(published.Data) {
		t.Fatalf("BundleRunBranch = {tip %s digest %s}, want tip %s", published.Tip, published.Digest, secondHead)
	}
	// Round trip: the worker's bundle lands in a fresh pod-style checkout.
	pod := filepath.Join(t.TempDir(), "pod2")
	runTestGit(t, filepath.Dir(pod), "clone", "-q", "--branch", "main", f.origin, pod)
	if tip, err := workspacedelta.Fetch(ctx, mirrorGit{}, pod, published); err != nil || tip != secondHead {
		t.Fatalf("Fetch worker bundle into a checkout = %s, %v; want %s", tip, err, secondHead)
	}

	// ahead -> keep: the stale first bundle must not rewind the mirror.
	stderr.Reset()
	out, err = f.manager.ApplyBundle(ctx, opts, first, &stderr)
	if err != nil {
		t.Fatalf("ApplyBundle (stale): %v", err)
	}
	if out.Outcome != workspacedelta.OutcomeKeep || out.After != secondHead {
		t.Fatalf("ApplyBundle (stale) = %+v, want keep at %s", out, secondHead)
	}
	if got := f.branchSHA(t, branch); got != secondHead {
		t.Fatalf("a stale delta rewound the mirror branch to %s (was %s)", got, secondHead)
	}
	if msg := stderr.String(); !strings.Contains(msg, secondHead) || !strings.Contains(msg, firstHead) {
		t.Fatalf("keep arm stderr = %q, want both SHAs named", msg)
	}

	// ff -> update-ref: a bundle strictly ahead of the mirror.
	pod3 := filepath.Join(t.TempDir(), "pod3")
	runTestGit(t, filepath.Dir(pod3), "clone", "-q", "--branch", "main", f.origin, pod3)
	runTestGit(t, pod3, "config", "user.name", "pod")
	runTestGit(t, pod3, "config", "user.email", "pod@example.com")
	runTestGit(t, pod3, "checkout", "-q", "-B", branch)
	if _, err := workspacedelta.Fetch(ctx, mirrorGit{}, pod3, published); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, pod3, "reset", "-q", "--hard", "FETCH_HEAD")
	mustWriteFile(t, filepath.Join(pod3, "third.txt"), "third\n")
	runTestGit(t, pod3, "add", "third.txt")
	runTestGit(t, pod3, "commit", "-q", "-m", "third")
	thirdHead := strings.TrimSpace(runTestGit(t, pod3, "rev-parse", "HEAD"))
	third, err := workspacedelta.Create(ctx, mirrorGit{}, pod3, "origin/main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	out, err = f.manager.ApplyBundle(ctx, opts, third, nil)
	if err != nil {
		t.Fatalf("ApplyBundle (ff): %v", err)
	}
	if out.Outcome != workspacedelta.OutcomeFastForward || out.Before != secondHead || out.After != thirdHead {
		t.Fatalf("ApplyBundle (ff) = %+v, want fast-forward %s -> %s", out, secondHead, thirdHead)
	}
	if got := f.branchSHA(t, branch); got != thirdHead {
		t.Fatalf("mirror %s = %s, want %s", branch, got, thirdHead)
	}

	// diverged -> named error, branch untouched.
	diverged, divergedHead := f.publishFrom(t, branch, "elsewhere.txt")
	_, err = f.manager.ApplyBundle(ctx, opts, diverged, nil)
	var divErr *workspacedelta.DivergedError
	if !errors.As(err, &divErr) {
		t.Fatalf("ApplyBundle (diverged) = %v, want DivergedError", err)
	}
	if divErr.Current != thirdHead || divErr.Tip != divergedHead {
		t.Fatalf("DivergedError = %+v, want current %s tip %s", divErr, thirdHead, divergedHead)
	}
	if got := f.branchSHA(t, branch); got != thirdHead {
		t.Fatalf("a diverged delta moved the mirror branch to %s", got)
	}
}

// Base drift on the mirror: the run branch was created at an OLD base by a
// worker-side consumer that ran before any producer, then a pod bundles from
// a newer base. The branch is nothing but base, so the delta applies.
func TestApplyBundleBaseDriftOnMirror(t *testing.T) {
	ctx := context.Background()
	const branch = "goobers/wf/run-drift"
	f := newBundleFixture(t)
	oldBase := strings.TrimSpace(runTestGit(t, f.origin, "rev-parse", "HEAD"))
	// Worker-side consumer created the run branch at base.
	wt, err := f.manager.Create(ctx, CreateOptions{RepoURL: f.origin, RunID: "run-drift-early", BaseRef: "main", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	// Base advances on origin; a pod bundles from the new base.
	mustWriteFile(t, filepath.Join(f.origin, "newmain.txt"), "new\n")
	runTestGit(t, f.origin, "add", "newmain.txt")
	runTestGit(t, f.origin, "commit", "-q", "-m", "unrelated main commit")
	b, head := f.publishFrom(t, branch, "carried.txt")

	out, err := f.manager.ApplyBundle(ctx, ApplyBundleOptions{RepoURL: f.origin, Branch: branch, BaseRef: "main"}, b, nil)
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	// oldBase is an ancestor of the new base, so this classifies as a plain
	// fast-forward; either way the branch must end at the delta tip.
	if out.Outcome != workspacedelta.OutcomeFastForward || out.Before != oldBase || out.After != head {
		t.Fatalf("ApplyBundle = %+v, want the branch moved from %s onto %s", out, oldBase, head)
	}
}

// The rebound-branch ordering: a delta for a branch that lives on ORIGIN
// (pr-remediation's workspaceBranch) must be reconciled against the
// acquired branch, not create it first and have Create's acquisition
// force-reset it to origin's older head.
func TestApplyBundleAcquiresRemoteBranchBeforeReconciling(t *testing.T) {
	ctx := context.Background()
	const branch = "goobers/pr/head"
	f := newBundleFixture(t)
	// The PR branch exists on origin with one commit.
	seed := filepath.Join(t.TempDir(), "seed")
	runTestGit(t, filepath.Dir(seed), "clone", "-q", f.origin, seed)
	runTestGit(t, seed, "config", "user.name", "seed")
	runTestGit(t, seed, "config", "user.email", "seed@example.com")
	runTestGit(t, seed, "checkout", "-q", "-b", branch)
	mustWriteFile(t, filepath.Join(seed, "pr.txt"), "pr\n")
	runTestGit(t, seed, "add", "pr.txt")
	runTestGit(t, seed, "commit", "-q", "-m", "pr")
	runTestGit(t, seed, "push", "-q", "origin", branch)
	prHead := strings.TrimSpace(runTestGit(t, seed, "rev-parse", "HEAD"))
	// A pod continued from it and bundled one more commit.
	mustWriteFile(t, filepath.Join(seed, "fix.txt"), "fix\n")
	runTestGit(t, seed, "add", "fix.txt")
	runTestGit(t, seed, "commit", "-q", "-m", "fix")
	fixHead := strings.TrimSpace(runTestGit(t, seed, "rev-parse", "HEAD"))
	b, err := workspacedelta.Create(ctx, mirrorGit{}, seed, "origin/main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.manager.ApplyBundle(ctx, ApplyBundleOptions{
		RepoURL: f.origin, Branch: branch, BaseRef: "main", OwnerRunID: "run-pr", AcquireRemoteBranch: true,
	}, b, nil)
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if out.Outcome != workspacedelta.OutcomeFastForward || out.Before != prHead || out.After != fixHead {
		t.Fatalf("ApplyBundle = %+v, want the acquired PR head %s fast-forwarded to %s", out, prHead, fixHead)
	}
	// Create's own acquisition must now be a no-op (marker present) and the
	// worktree must carry the delta, not origin's older head.
	wt, err := f.manager.Create(ctx, CreateOptions{
		RepoURL: f.origin, RunID: "run-pr-stage", OwnerRunID: "run-pr", BaseRef: "main", Branch: branch,
		RequireExistingBranch: true, AcquireRemoteBranch: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Remove(ctx, RemoveOptions{}) }()
	if head, _ := wt.HeadSHA(ctx); head != fixHead {
		t.Fatalf("worktree HEAD = %s, want the delta tip %s (Create's acquisition reset the branch to origin)", head, fixHead)
	}
}

func TestBundleRunBranchRefusesUnknownBranch(t *testing.T) {
	f := newBundleFixture(t)
	if _, err := f.manager.BundleRunBranch(context.Background(), f.origin, "goobers/wf/never", "main"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("BundleRunBranch = %v, want a missing-branch refusal", err)
	}
}

// TestBundleRunBranchNamesNothingToCarry: the two "nothing beyond base yet"
// shapes a continuity publisher meets before a run's first commit — no run
// branch in the mirror at all, and a run branch created at base with no
// commit on it (git refuses an empty bundle) — are reported as the named
// sentinels, distinct from every real fault.
func TestBundleRunBranchNamesNothingToCarry(t *testing.T) {
	ctx := context.Background()
	f := newBundleFixture(t)
	if _, err := f.manager.BundleRunBranch(ctx, f.origin, "goobers/wf/never", "main"); !errors.Is(err, ErrRunBranchAbsent) {
		t.Fatalf("absent branch: %v, want ErrRunBranchAbsent", err)
	}
	if _, err := f.manager.BundleRunBranch(ctx, filepath.Join(t.TempDir(), "never-mirrored.git"), "goobers/wf/never", "main"); !errors.Is(err, ErrRunBranchAbsent) {
		t.Fatalf("absent mirror: %v, want ErrRunBranchAbsent", err)
	}
	const branch = "goobers/wf/run-empty"
	wt, err := f.manager.Create(ctx, CreateOptions{RepoURL: f.origin, RunID: "run-empty-stage", OwnerRunID: "run-empty", BaseRef: "main", Branch: branch})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := f.manager.BundleRunBranch(ctx, f.origin, branch, "main"); !errors.Is(err, ErrRunBranchUnchanged) {
		t.Fatalf("branch at base: %v, want ErrRunBranchUnchanged", err)
	}
	if _, err := f.manager.BundleRunBranch(ctx, f.origin, branch, "main"); errors.Is(err, ErrRunBranchAbsent) {
		t.Fatalf("branch at base reported absent: %v", err)
	}
}
