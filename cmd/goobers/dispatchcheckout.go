package main

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
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/mergeresolve"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// dispatchcheckout.go provisions a repo workspace inside a stage pod.
//
// It deliberately MIRRORS the semantics of the worker-side provisioner
// (internal/workerhost.WorktreeWorkspaces) rather than inventing a transport.
// That provisioner gives each stage attempt a FRESH worktree on the RUN BRANCH
// and fetches that branch from the remote when one is selected — the mirror
// clone it keeps is a host-local cache, not the mechanism. So the run branch,
// not a shared filesystem, is already how work moves between stages, and a pod
// that clones the run branch is doing the same thing by a different route.
//
// This is what makes a stage declaring `workspace: repo` dispatchable at all:
// before it, mode 3 refused every such stage, which is every stage in a
// workflow that does not explicitly opt into scratch.
//
// TWO THINGS ARE STAMPED RATHER THAN DERIVED, and both are #392: the run's
// REBOUND workspace branch, and whether the stage asked for its base to be
// synced. Everything else about the checkout follows from the run identity the
// dispatcher already stamps, but neither of these can: the branch a
// pr-remediation run works on is the claimed PR's head, which only the provider
// knows and which no composition of workflow + run id yields, and syncBase is a
// per-stage DSL declaration that lives in the pinned definition. Deriving in
// their absence — which is what this file did — put every pod stage of
// pr-remediation on a run branch nobody was reviewing, against a base as old as
// the PR.

// checkoutCloneURL derives the git remote for a routed repository. A package
// var solely so a test can point the real clone path at a local bare repo:
// without it the only coverage for the code that actually does the work is a
// live cluster, which is how #3734's two bugs reached one.
var checkoutCloneURL = runner.DefaultRepoCloneURL

// checkoutRepoWorkspace clones the run's repository into dir when the stage
// declared a repo workspace. It is a no-op for scratch, which keeps the
// pre-checkout behaviour byte-identical for stages that never needed it.
func checkoutRepoWorkspace(ctx context.Context, dir string, stderr io.Writer, creds []dispatcher.MintedCredential) error {
	mode := strings.TrimSpace(os.Getenv(dispatcher.EnvStageWorkspace))
	if mode == "" || mode == string(apiv1.WorkspaceScratch) {
		return nil
	}
	wsMode := apiv1.WorkspaceMode(mode)
	if !wsMode.IsRepoBacked() {
		return fmt.Errorf("unsupported workspace mode %q", mode)
	}

	ref := apiv1.RepoRef{
		Provider: apiv1.Provider(os.Getenv(executor.RepoProviderEnvVar)),
		Owner:    os.Getenv(executor.RepoOwnerEnvVar),
		Project:  os.Getenv(executor.RepoProjectEnvVar),
		Name:     os.Getenv(executor.RepoNameEnvVar),
	}
	if ref.Provider == "" || ref.Name == "" {
		return fmt.Errorf("repo workspace requested but the dispatcher stamped no repository")
	}
	cloneURL, err := checkoutCloneURL(ref)
	if err != nil {
		return fmt.Errorf("derive clone URL: %w", err)
	}

	namespace := providers.NormalizeBranchNamespace(os.Getenv(executor.BranchNamespaceEnvVar))
	// The run branch is DERIVED here, from the same inputs and the same helper
	// the worker uses, rather than stamped. Two derivations of one convention
	// cannot disagree about the branch a run lives on; two stamped strings can.
	branch := providers.BranchNameIn(
		namespace,
		os.Getenv(dispatcher.EnvWorkflow),
		os.Getenv(dispatcher.EnvRunID),
	)
	// ... EXCEPT when the run rebound its workspace branch (#392). That one
	// cannot be derived: the derivation above composes the RUN branch, and a
	// rebound run is by definition not on it — pr-remediation binds the
	// binding to the claimed PR's head, which only the provider knows. The
	// dispatcher stamps it; deriving anything here would put the stage on the
	// wrong tree while looking entirely healthy.
	rebound := strings.TrimSpace(os.Getenv(dispatcher.EnvWorkspaceBranch))
	if rebound != "" {
		// Re-assert the namespace the engine already enforced when it accepted
		// the rebinding (selectedWorkspaceBranch). Defense in depth, and cheap:
		// this string decides which branch a later stage force-pushes with a
		// lease, so "outside the namespace" must fail here rather than reach
		// git.
		if !strings.HasPrefix(rebound, namespace) {
			return fmt.Errorf("stamped workspace branch %q is outside branch namespace %q; refusing to provision it", rebound, namespace)
		}
		branch = rebound
	}
	base := strings.TrimSpace(os.Getenv(executor.BaseBranchEnvVar))
	if base == "" {
		base = "main"
	}

	gitEnv, err := checkoutGitAuthEnv(dir, creds)
	if err != nil {
		return err
	}

	// repo-readonly is DETACHED AT BASE and must not see the run branch. The
	// worker-side reason for detaching is a git constraint that does not exist
	// here (one branch cannot be checked out in two worktrees; separate pods
	// have separate filesystems), but the SEMANTICS still hold and are the
	// point: a read-only research stage reads the pinned base revision, not
	// whatever earlier stages have pushed to the run branch. Provisioning it on
	// the run branch would quietly change what such a stage reads.
	if !wsMode.IsWritableRepo() {
		if err := runGit(ctx, dir, gitEnv, stderr, "clone", "--quiet", "--branch", base, cloneURL, "."); err != nil {
			return fmt.Errorf("clone %s at %s: %w", cloneURL, base, err)
		}
		if err := runGit(ctx, dir, gitEnv, stderr, "checkout", "--quiet", "--detach"); err != nil {
			return fmt.Errorf("detach at %s: %w", base, err)
		}
		return nil
	}

	// Writable repo: prefer the RUN BRANCH. If an earlier stage of this run
	// already pushed to it, that is the state this stage must continue from —
	// which is exactly how continuity works on the worker side, where each
	// attempt gets a fresh worktree on the same branch.
	//
	// A PUSHED run branch is not the only way work arrives, and assuming it was
	// is what #3763 measured: the universal idiom commits in one stage and
	// pushes in a later one, so the common case is unpushed commits that must
	// still reach this stage. applyStageWorkspaceDelta covers that.
	if err := runGit(ctx, dir, gitEnv, stderr, "clone", "--quiet", "--branch", branch, cloneURL, "."); err == nil {
		if err := applyStageWorkspaceDelta(ctx, dir, gitEnv, stderr); err != nil {
			return err
		}
		// Base sync AFTER the delta, matching the self runner's order: the
		// worker/worktree provisioner applies the delta into the mirror and
		// only then creates the worktree with SyncBase, so the merge lands on
		// the branch as the stage will see it. Reversing the two would merge
		// base into a branch the delta is about to reset away from.
		return syncWorkspaceBase(ctx, dir, gitEnv, stderr, branch, base)
	}
	// A REBOUND branch that does not exist is a refusal, not a fallback (#392).
	// The fallback below creates the branch locally at base, which is right for
	// the first stage of a run — the run branch legitimately does not exist yet
	// — and catastrophically wrong for a rebound one: the branch was named by a
	// producer stage that read it off a real, open PR, so its absence means the
	// premise broke. Falling back would hand the stage a pristine base checkout
	// wearing the PR's branch name, and push-remediated would then force-push
	// THAT over the PR head with a lease. The local runner refuses the same
	// case (createStageWorkspace's RequireExistingBranch, set exactly when the
	// branch was rebound); this is that refusal on the pod substrate.
	if rebound != "" {
		return fmt.Errorf("rebound workspace branch %q does not exist at %s; refusing to create it at base — the branch names work that already exists", rebound, cloneURL)
	}
	// First stage of the run: the branch does not exist yet.
	//
	// The attempt above may have left partial state — git can populate the
	// destination and only then discover the branch is missing — and a second
	// `clone <url> .` into a non-empty directory is refused outright. Clearing
	// first makes the fallback independent of how far the first attempt got,
	// rather than depending on git's cleanup behaviour differing between a
	// local file:// remote and an authenticated HTTPS one.
	if err := clearDirContents(dir); err != nil {
		return fmt.Errorf("clear workspace before fallback clone: %w", err)
	}
	if err := runGit(ctx, dir, gitEnv, stderr, "clone", "--quiet", "--branch", base, cloneURL, "."); err != nil {
		return fmt.Errorf("clone %s at %s: %w", cloneURL, base, err)
	}
	// Create it locally so a stage that commits is already on it, matching the
	// worktree the worker would have handed a self-placed stage.
	if err := runGit(ctx, dir, gitEnv, stderr, "checkout", "--quiet", "-b", branch); err != nil {
		return fmt.Errorf("create run branch %s: %w", branch, err)
	}
	return applyStageWorkspaceDelta(ctx, dir, gitEnv, stderr)
}

// applyStageWorkspaceDelta moves the freshly-checked-out branch onto whatever
// earlier stages of this run committed, when the dispatcher handed this pod a
// delta to apply (#3763). No delta stamped means nothing to continue from —
// the first writable stage of a run, or a run whose earlier stages committed
// nothing — and the base checkout already standing is correct.
func applyStageWorkspaceDelta(ctx context.Context, dir string, gitEnv []string, stderr io.Writer) error {
	digest := strings.TrimSpace(os.Getenv(dispatcher.EnvWorkspaceDelta))
	if digest == "" {
		return nil
	}
	return applyWorkspaceDelta(ctx, dir, digest, gitEnv, stderr)
}

// syncWorkspaceBase merges the freshly fetched base into the branch the
// checkout landed on, when the stage declared run.syncBase (#813).
//
// It is the pod's half of a SHIPPED DSL feature that had no pod-side path at
// all: the local runner and the worker both honour syncBase through
// worktree.CreateOptions.SyncBase, so a `syncBase: true` stage was correct on
// self and silently unsynced in a pod — running its build against a base
// weeks behind the one the self runner would have given it. pr-remediation's
// rebase-pr is exactly such a stage.
//
// The semantics are the provisioner's, not a new invention: `git merge --ff
// --no-edit <base>` (a fast-forward when the branch has no commits of its own,
// a merge commit when it does), the same mechanical adjacent-line conflict
// resolution the worktree path applies, and the same fail-closed outcome when
// the conflict is real — a stage that cannot sync base must not proceed
// against a stale one and call it success.
//
// Called only on the arm where the branch ALREADY EXISTED on the remote,
// mirroring the provisioner's `SyncBase && existingBranch` condition: a branch
// this pod just created at base is synced by construction.
func syncWorkspaceBase(ctx context.Context, dir string, gitEnv []string, stderr io.Writer, branch, base string) error {
	if strings.TrimSpace(os.Getenv(dispatcher.EnvStageSyncBase)) != "true" {
		return nil
	}
	// FETCH, then merge FETCH_HEAD: "freshly fetched" is the contract's word
	// (WorkspaceRequest.SyncBase), and the remote-tracking ref a clone left
	// behind is only as fresh as the clone. Merging FETCH_HEAD rather than
	// origin/<base> also keeps this correct on a single-branch clone, where no
	// remote-tracking ref for the base exists at all.
	if err := runGit(ctx, dir, gitEnv, stderr, "fetch", "--quiet", "origin", base); err != nil {
		return fmt.Errorf("syncBase: fetch base %s: %w", base, err)
	}
	// A branch with commits of its own gets a MERGE COMMIT, and git refuses to
	// write one without an identity. A pod's clone has none — the worktree
	// provisioner sets one on every worktree it creates and a fresh clone
	// inherits only whatever the image happens to carry — so the same identity
	// is set here, from the same constants, rather than depending on ambient
	// image config. Local to this repository, exactly as worktree.Create does
	// it. Scoped to the syncBase arm on purpose: every other pod checkout
	// behaves precisely as it did before this existed.
	for _, kv := range [][2]string{{"user.name", worktree.BotGitUserName}, {"user.email", worktree.BotGitUserEmail}} {
		if err := runGit(ctx, dir, gitEnv, stderr, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("syncBase: set commit identity %s: %w", kv[0], err)
		}
	}
	if err := runGit(ctx, dir, gitEnv, stderr, "merge", "--ff", "--no-edit", "FETCH_HEAD"); err != nil {
		resolved, resolveErr := resolveWorkspaceBaseSyncConflict(ctx, dir, gitEnv, stderr)
		if !resolved {
			files, inspectErr := workspaceMergeConflictFiles(ctx, dir, gitEnv)
			// Leave no half-merged tree behind: the stage runs in this very
			// directory, and a workspace full of conflict markers is a far
			// worse failure than the merge itself.
			_ = runGit(ctx, dir, gitEnv, stderr, "merge", "--abort")
			// JOINED, not formatted with %v (internal/worktree's baseSyncFailure
			// idiom): the merge failure is the cause a reader acts on, and the
			// inspect/resolve failures are the reasons the message is thinner
			// than it should be. Flattening those two to text would make an
			// already-degraded diagnosis unmatchable by errors.Is.
			return fmt.Errorf("syncBase: merge base %s into %s (conflicting files: %s): %w",
				base, branch, strings.Join(files, ", "), errors.Join(err, inspectErr, resolveErr))
		}
	}
	pf(stderr, "workspace: synced base %s into %s\n", base, branch)
	return nil
}

// resolveWorkspaceBaseSyncConflict applies the SHARED mechanical resolution
// (internal/mergeresolve, the one internal/worktree's syncBase path uses) to a
// conflicted base merge and commits it when every unmerged path was provably
// safe. Anything else leaves the merge conflicted for the caller to abort.
func resolveWorkspaceBaseSyncConflict(ctx context.Context, dir string, gitEnv []string, stderr io.Writer) (bool, error) {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = composeGitEnv(dir, gitEnv)
		return cmd.Output()
	}
	status, err := mergeresolve.ResolveAdjacentLineConflicts(dir, git)
	if err != nil || status != mergeresolve.StatusResolved {
		return false, err
	}
	if err := runGit(ctx, dir, gitEnv, stderr, "commit", "--no-edit"); err != nil {
		return false, fmt.Errorf("commit mechanically resolved base merge: %w", err)
	}
	return true, nil
}

// workspaceMergeConflictFiles names the unmerged paths, so the surrendered
// error says WHICH files collided. The pod is disposed as soon as the stage
// fails, so a message that omits them is unactionable.
func workspaceMergeConflictFiles(ctx context.Context, dir string, gitEnv []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U", "-z")
	cmd.Dir = dir
	cmd.Env = composeGitEnv(dir, gitEnv)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, file := range strings.Split(string(out), "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// gitAuthEnv builds the git child environment from a credential the stage
// already declared. No new credential surface: the workspace is provisioned
// with what the stage was granted, and a stage that declared nothing gets an
// anonymous clone — which is correct for a public repository and fails at the
// clone with git's own message for a private one.
func checkoutGitAuthEnv(dir string, creds []dispatcher.MintedCredential) ([]string, error) {
	token := gitToken(creds)
	if token == "" {
		return nil, nil
	}
	// OUTSIDE the workspace, deliberately. `git clone <url> .` refuses a
	// non-empty destination, so a helper written into the workspace makes the
	// clone fail before it starts:
	//   fatal: destination path '.' already exists and is not an empty directory
	// The worker-side path has always kept this in a control directory beside
	// the worktree rather than inside it (workcopies/auth); this is the same
	// separation, and the reason for it is now recorded where it bites.
	authDir, err := os.MkdirTemp("", "goobers-auth-*")
	if err != nil {
		return nil, fmt.Errorf("create auth dir: %w", err)
	}
	askpass, err := credentials.WriteAskpassScript(authDir)
	if err != nil {
		return nil, fmt.Errorf("write askpass helper: %w", err)
	}
	// The token reaches git ONLY through this child's environment — the helper
	// script holds no secret, which is the property internal/credentials exists
	// to preserve.
	return credentials.GitAuthEnvironment(askpass, token), nil
}

// runGit runs one git command and returns an error carrying GIT'S OWN message.
//
// Returning a bare exit status was a mistake worth naming: "create run branch
// <name>: exit status 128" is unactionable, and the pod is disposed as soon as
// the stage fails, so the stderr that would have explained it is gone before it
// can be read. Every other failure path added tonight names its cause; this one
// did not, and that cost a full deploy cycle to diagnose.
func runGit(ctx context.Context, dir string, env []string, stderr io.Writer, args ...string) error {
	var captured strings.Builder
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// GIT_TERMINAL_PROMPT=0 is not cosmetic: without it a clone of a private
	// repository with no usable credential can BLOCK asking for one. A stage
	// pod has no terminal, so the ask would hang until the stage timed out and
	// be reported as a timeout rather than as the auth failure it is.
	cmd.Env = composeGitEnv(dir, env)
	cmd.Stdout = io.Discard
	// Tee: the pod's stderr keeps the live view, and the copy rides the error
	// so the surrendered envelope is self-describing after disposal.
	cmd.Stderr = io.MultiWriter(stderr, &captured)
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(captured.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// gitToken picks a credential the stage already holds that can authenticate a
// git fetch. Preference order matters: a write-scoped credential also reads, so
// either works for the clone, but choosing deterministically keeps the same
// credential in play across attempts of the same stage.
func gitToken(creds []dispatcher.MintedCredential) string {
	for _, c := range creds {
		name := strings.ToLower(c.Capability)
		if c.Value == "" {
			continue
		}
		// "repo" ONLY. Matching "git" as well looked harmless and was not:
		// "github:issues:read" contains it, so an issues-only token would have
		// been handed to git — the exact scope widening the comment below says
		// this must not do. Caught by
		// TestCheckoutRefusesToSubstituteAnUnrelatedCredential.
		//
		// "repo" covers the capabilities production stages actually declare:
		// repo:push, push-repository-branch, modify-repository.
		if strings.Contains(name, "repo") {
			return c.Value
		}
	}
	// No repo-shaped capability. Deliberately NOT falling back to any other
	// credential the stage happens to hold: handing an issues-read token to git
	// would be a silent scope widening. An anonymous clone is correct for a
	// public repository and fails loudly on a private one, which is the honest
	// outcome and names the real cause.
	return ""
}

// clearDirContents empties dir without removing dir itself: the workspace is a
// mount point, so it must stay in place.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// workspaceGitEnv marks the workspace as a safe git directory.
//
// A stage pod runs as a non-root uid while its workspace volume is created
// root-owned, so git refuses every command after the clone:
//
//	fatal: detected dubious ownership in repository at '/workspace'
//
// MEASURED on a live cluster — the clone SUCCEEDS (it is creating files) and
// the very next command fails, which is why this surfaced as a branch-creation
// failure rather than a checkout one.
//
// Scoped to the workspace PATH, never the "*" wildcard: the protection exists
// to stop git trusting a repository someone else planted, and a pod that
// disabled it globally would trust anything mounted into it. Delivered through
// the environment rather than a config file so the STAGE'S OWN git inherits it
// too — real workflows run git directly (goobers push-branch commits and
// pushes from this very tree), so fixing only our own invocations would move
// the failure one step later instead of removing it.
// composeGitEnv builds the git child environment from the auth environment (if
// any) PLUS the workspace's safe.directory exemption.
//
// It composes rather than appends because credentials.GitAuthEnvironment
// returns a COMPLETE environment and deliberately strips foreign GIT_CONFIG_*
// entries before installing its own (COUNT=1, KEY_0=credential.helper).
// Appending it after a safe.directory setting silently erased that setting —
// MEASURED: the clone succeeded and every later git command still failed with
// "detected dubious ownership", because the exemption was gone by the time git
// ran. Two independent settings both wanting slot 0 is the trap; the count has
// to be extended, not restated.
func composeGitEnv(dir string, authEnv []string) []string {
	safe := workspaceGitEnv(dir)
	if len(authEnv) == 0 {
		// No credential: strip any inherited GIT_CONFIG_* so our indices are
		// the only ones, then claim slot 0.
		env := make([]string, 0, len(os.Environ())+4)
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			upper := strings.ToUpper(name)
			if upper == "GIT_CONFIG_COUNT" ||
				strings.HasPrefix(upper, "GIT_CONFIG_KEY_") ||
				strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
				continue
			}
			env = append(env, entry)
		}
		return append(env,
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=safe.directory",
			"GIT_CONFIG_VALUE_0="+safe.path,
		)
	}
	// With auth: keep GitAuthEnvironment's slot 0 (credential.helper) and take
	// slot 1, raising the count. Later entries win, so the new count replaces
	// the one it set.
	return append(append([]string{}, authEnv...),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_1=safe.directory",
		"GIT_CONFIG_VALUE_1="+safe.path,
	)
}

func workspaceGitEnv(dir string) safeDirectory {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return safeDirectory{path: abs}
}

// safeDirectory carries the resolved workspace path plus the env form the STAGE
// inherits, so the two cannot describe different directories.
type safeDirectory struct{ path string }

// Env renders the exemption for a child that has no other GIT_CONFIG_* of its
// own — the stage's command.
func (s safeDirectory) Env() []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=" + s.path,
	}
}
