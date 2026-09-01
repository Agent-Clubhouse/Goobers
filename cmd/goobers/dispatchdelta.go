package main

// Cross-stage workspace continuity for mode 3 (#3763), pod side.
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
// The bundle mechanics and the ancestry guard (#3821) live in
// internal/workspacedelta, shared with the worker's mirror (#3803): this file
// is the pod's adapter — where the bytes come from (the blob plane), which git
// environment every call needs (composeGitEnv's safe.directory exemption),
// and what to do with the guard's verdict on a CHECKOUT (reset --hard, not
// update-ref).
//
// WHY NOT PUSH TO THE RUN BRANCH. It is simpler and it changes observable
// semantics: commits would become visible on the remote before the gates that
// guard them run, open-pr could find the branch already present, and
// pr-remediation's force-push-with-lease would have its lease baseline moved.
// The run branch stays written by push-branch alone.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workspacedelta"
)

// workspaceDeltaRef is the ref name the bundle carries its commits under —
// the shared package's, restated for the tests that check it is not left
// behind in the source repo.
const workspaceDeltaRef = workspacedelta.Ref

// publishBaseFile is where dispatch-checkout leaves HEAD as it HANDED the
// workspace to the stage, and where publishWorkspaceDelta reads it back
// (#4124). It is the pod's half of the worker's worktreeWorkspace.publishBase
// field, which exists for the same reason and answers the same question.
//
// It lives under .git/ because the two halves are separate PROCESSES in one
// pod — dispatch-checkout provisions, dispatch-exec runs and publishes — so an
// in-memory field cannot span them, and anything in the working tree would
// show up in the stage's own `git status`, in its diff, and in the bundle this
// very mechanism cuts. .git/ is inside the workspace, outside the working
// tree, and disposed with the pod.
const publishBaseFile = "goobers-publish-base"

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

// podGit is workspacedelta.Git for an in-pod checkout: every call carries the
// workspace's safe.directory exemption (composeGitEnv), because /workspace is
// not owned by the container user and bare git refuses it with "detected
// dubious ownership" — MEASURED, and the reason every git call in this path
// goes through here. env is the checkout's auth environment when the caller
// has one; stderr keeps the pod's live view.
type podGit struct {
	env    []string
	stderr io.Writer
}

func (g podGit) Run(ctx context.Context, dir string, args ...string) error {
	return runGit(ctx, dir, g.env, g.stderr, args...)
}

// Output captures git's own stderr into the error rather than discarding it:
// the pod is disposed as soon as the stage fails, and this message is all
// that survives to explain why.
func (g podGit) Output(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = composeGitEnv(dir, g.env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %v: %w: %s", args, err, msg)
		}
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// publishedWorkspaceDelta is what a pod surrenders about the delta it
// published: the digest the engine threads and the two SHAs it journals — or
// Unchanged, the pod's own finding that its writable repo branch carries no
// commits beyond base. Unchanged is reported, not left for the engine to
// infer from an empty Digest, because an empty Digest is also what a scratch
// stage or an older stage image surrenders, and neither of those has checked
// the branch.
type publishedWorkspaceDelta struct {
	Digest    string
	Base      string
	Tip       string
	Unchanged bool
}

// publishWorkspaceDelta bundles whatever this stage committed beyond the run's
// base and publishes it to the blob plane, returning the digest the next stage
// needs. It returns a zero value when there is nothing to carry — not a repo
// workspace, or a repo workspace with no new commits — both of which are
// ordinary.
//
// "No new commits" is measured against the HEAD this stage was HANDED, not
// against base (#4124). The two differ constantly and the difference is not
// cosmetic: a stage that applied a predecessor's delta, or that stands on a
// rebound PR head, is ahead of base before it runs a single command, so a
// base-only test says "publish" for every stage that changed nothing. The
// engine then records a non-producer as the continuity record's newest entry
// and refuses the next declared 3.0 consumer over it (WF022 runtime, #3767) —
// MEASURED as run 187c9f340bf9 on PR #3900, where gather-pr-context's bundle
// was re-published verbatim by rebase-pr (which aborted a conflicted rebase)
// and again by remediation-checkpoint (which only writes a comment), and
// implement was refused for not declaring a producer that had produced
// nothing. This is the same question workerhost's PublishDelta asks of
// publishBase; asking it here is what makes the two substrates agree.
//
// A failure here is NOT ordinary and is returned as an error: the commits exist,
// nothing else will carry them, and reporting success would strand exactly the
// diff this exists to preserve.
func publishWorkspaceDelta(ctx context.Context, dir string, stderr io.Writer) (publishedWorkspaceDelta, error) {
	if !stageWorkspaceIsWritableRepo() {
		return publishedWorkspaceDelta{}, nil
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
		return publishedWorkspaceDelta{}, fmt.Errorf("workspace delta: cannot determine the checked-out branch of the writable repo workspace: %w", err)
	}
	if moved, known := stageCommittedBeyondPublishBase(dir, stderr); known && !moved {
		return publishedWorkspaceDelta{Unchanged: true}, nil
	}
	empty, err := branchHasNoCommitsBeyondBase(dir, branch)
	if err != nil {
		// Cannot prove there is nothing to carry, so do not claim there isn't.
		// Falling through to bundle is the safe direction: an empty bundle is
		// wasteful, a skipped one loses work.
		pf(stderr, "workspace delta: could not count commits on %q (%v); bundling anyway\n", branch, err)
	} else if empty {
		return publishedWorkspaceDelta{Unchanged: true}, nil
	}

	client := podBlobClient()
	if client == nil {
		return publishedWorkspaceDelta{}, fmt.Errorf("stage committed to %s but this pod has no %s; the commits cannot reach the next stage", branch, dispatcher.EnvBlobEndpoint)
	}
	baseRef, err := resolveBaseRef(dir, stageBaseBranch())
	if err != nil {
		return publishedWorkspaceDelta{}, fmt.Errorf("workspace delta: %w", err)
	}
	bundle, err := workspacedelta.Create(ctx, podGit{stderr: stderr}, dir, baseRef, "HEAD")
	if err != nil {
		return publishedWorkspaceDelta{}, err
	}
	if err := client.Put(ctx, bundle.Digest, bundle.Data); err != nil {
		return publishedWorkspaceDelta{}, fmt.Errorf("workspace delta: publish %s (%d bytes): %w", bundle.Digest, len(bundle.Data), err)
	}
	pf(stderr, "workspace delta: published %s (%d bytes) carrying %s..%s (%s..%s)\n", bundle.Digest, len(bundle.Data), baseRef, branch, bundle.Base, bundle.Tip)
	return publishedWorkspaceDelta{Digest: bundle.Digest, Base: bundle.Base, Tip: bundle.Tip}, nil
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
// The ancestry guard is workspacedelta.Reconcile (#3821, shared with the
// worker's mirror): fast-forward or equal -> reset onto the tip; the delta
// strictly behind HEAD -> keep the checkout and say so with both SHAs; HEAD
// merely an advanced base (the base-fallback clone landed on a newer
// origin/<base> than the delta was bundled from) -> apply as fast-forward;
// genuine divergence -> fail closed naming both SHAs. A REWRITTEN run branch
// (a rebase-pr self-placement, any force-push) lands in the last arm and
// fails closed too: recognising rebase-equivalent history is a weaker safety
// argument than ancestry and is deliberately not attempted.
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
	bundle, err := workspacedelta.Load(data, digest)
	if err != nil {
		return err
	}
	git := podGit{env: gitEnv, stderr: stderr}
	tip, err := workspacedelta.Fetch(ctx, git, dir, bundle)
	if err != nil {
		// A THIN bundle needs its prerequisite commit present in the receiving
		// repository; git refuses the fetch with "does not contain prerequisite
		// commits" when it is not. On one branch that means the base moved
		// under the run. Across TWO branches (#392) it means something sharper
		// and worth naming separately: the delta was bundled on the run branch
		// before the run rebound its workspace to a PR head (or the reverse),
		// so its prerequisite is on a line of history this checkout does not
		// contain at all. The engine's continuity selector keys the record on
		// the branch and should never hand such a digest across a rebind; this
		// is the pod re-asserting it at the substrate, where git's own message
		// alone would send a reader hunting a base drift that never happened.
		return fmt.Errorf("%w%s", err, deltaBranchContext(dir))
	}
	head, err := git.Output(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("workspace delta %s: determine current HEAD: %w", digest, err)
	}
	// A failure resolving base is NOT treated as "assume drift": Reconcile
	// falls through to the named divergence error, matching every other
	// uncertain branch in this path.
	baseRef, _ := resolveBaseRef(dir, stageBaseBranch())
	outcome, err := workspacedelta.Reconcile(ctx, git, dir, digest, head, tip, baseRef)
	if err != nil {
		// The shared guard already names both SHAs; what it cannot know is
		// WHICH branch this pod is standing on, which is the first thing a
		// reader of a diverged pr-remediation run needs (#392).
		return fmt.Errorf("%w%s", err, deltaBranchContext(dir))
	}
	switch outcome {
	case workspacedelta.OutcomeKeep:
		// The checkout already carries everything the delta does and then
		// some (a self-placed stage or a provider-side producer such as
		// update-behind-pr advanced the branch after this digest was
		// published). Resetting onto it would rewind that work. The stage's
		// own stderr — the only record that survives pod disposal — carries
		// the far-side evidence.
		pf(stderr, "workspace delta is behind the checkout; keeping %s (delta %s carries %s)\n", head, digest, tip)
		return nil
	case workspacedelta.OutcomeBaseDrift:
		pf(stderr, "workspace delta %s: checkout %s is an advanced base, not a diverged run (base moved past the delta's prerequisite mid-run); applying delta carrying %s\n", digest, head, tip)
	}
	if err := runGit(ctx, dir, gitEnv, stderr, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("move onto workspace delta %s: %w", digest, err)
	}
	return nil
}

// deltaBranchContext is the branch half of a delta failure's message: which
// branch the workspace is actually on, and whether that branch is one the run
// REBOUND to (#392) rather than the run branch a pod derives for itself.
//
// It returns a leading-space suffix, or "" when neither fact is available —
// appending nothing is better than appending a lie, and a checkout too broken
// to answer `symbolic-ref` has already produced a louder error of its own.
func deltaBranchContext(dir string) string {
	var parts []string
	if branch, err := currentBranch(dir); err == nil && branch != "" {
		parts = append(parts, fmt.Sprintf("the workspace is on branch %q", branch))
	}
	if rebound := strings.TrimSpace(os.Getenv(dispatcher.EnvWorkspaceBranch)); rebound != "" {
		parts = append(parts, fmt.Sprintf("this run rebound its workspace branch to %q, so a delta produced before the rebind cannot land here", rebound))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// publishBasePath is where the handed-HEAD baseline lives for the workspace
// rooted at dir. Resolved through git rather than assumed to be dir/.git, so a
// worktree-style checkout (where .git is a FILE naming the real directory)
// records and reads the same place.
func publishBasePath(dir string) (string, error) {
	out, err := workspaceGitCommand(dir, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("resolve git dir for the publish baseline: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("resolve git dir for the publish baseline: git reported no path")
	}
	return filepath.Join(gitDir, publishBaseFile), nil
}

// recordStagePublishBase writes HEAD as the stage is about to receive it.
//
// Called at the END of provisioning a writable repo workspace — after the
// clone, after any inherited delta was applied, after any syncBase merge —
// because every one of those moves HEAD without the stage having done
// anything, which is precisely what publishWorkspaceDelta must not mistake for
// the stage's own work.
//
// A failure is reported to the caller. Provisioning that cannot record the
// baseline would leave publishWorkspaceDelta with no way to tell an inherited
// commit from a produced one, and the silent shape that follows — a
// non-producer entering the continuity record — is what #4124 is.
func recordStagePublishBase(ctx context.Context, dir string, gitEnv []string, stderr io.Writer) error {
	git := podGit{env: gitEnv, stderr: stderr}
	head, err := git.Output(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("record the publish baseline: determine provisioned HEAD: %w", err)
	}
	path, err := publishBasePath(dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(head)+"\n"), 0o600); err != nil {
		return fmt.Errorf("record the publish baseline: %w", err)
	}
	return nil
}

// stageCommittedBeyondPublishBase reports whether HEAD has moved past the
// baseline provisioning recorded, and whether that could be established at all.
//
// known=false is the FAIL-OPEN arm and it is deliberate. No baseline means an
// image whose dispatch-checkout predates #4124, or a workspace provisioned by
// some path that does not record one; in either case the caller must fall
// through to the base-range test it used before, because publishing a
// redundant bundle wastes bytes while skipping a real one loses a stage's
// work. An unreadable HEAD is the same: unproven, not proven-unchanged.
func stageCommittedBeyondPublishBase(dir string, stderr io.Writer) (moved, known bool) {
	path, err := publishBasePath(dir)
	if err != nil {
		pf(stderr, "workspace delta: %v; measuring against base instead\n", err)
		return false, false
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			pf(stderr, "workspace delta: could not read the publish baseline (%v); measuring against base instead\n", err)
		}
		return false, false
	}
	baseline := strings.TrimSpace(string(recorded))
	if baseline == "" {
		return false, false
	}
	out, err := workspaceGitCommand(dir, "rev-parse", "--verify", "HEAD^{commit}").Output()
	if err != nil {
		pf(stderr, "workspace delta: could not resolve HEAD against the publish baseline (%v); measuring against base instead\n", err)
		return false, false
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return false, false
	}
	return head != baseline, true
}
