package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/runner"
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

	// The run branch is DERIVED here, from the same inputs and the same helper
	// the worker uses, rather than stamped. Two derivations of one convention
	// cannot disagree about the branch a run lives on; two stamped strings can.
	branch := providers.BranchNameIn(
		providers.NormalizeBranchNamespace(os.Getenv(executor.BranchNamespaceEnvVar)),
		os.Getenv(dispatcher.EnvWorkflow),
		os.Getenv(dispatcher.EnvRunID),
	)
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
	if err := runGit(ctx, dir, gitEnv, stderr, "clone", "--quiet", "--branch", branch, cloneURL, "."); err == nil {
		return nil
	}
	// First stage of the run: the branch does not exist yet.
	if err := runGit(ctx, dir, gitEnv, stderr, "clone", "--quiet", "--branch", base, cloneURL, "."); err != nil {
		return fmt.Errorf("clone %s at %s: %w", cloneURL, base, err)
	}
	// Create it locally so a stage that commits is already on it, matching the
	// worktree the worker would have handed a self-placed stage.
	if err := runGit(ctx, dir, gitEnv, stderr, "checkout", "--quiet", "-b", branch); err != nil {
		return fmt.Errorf("create run branch %s: %w", branch, err)
	}
	return nil
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

func runGit(ctx context.Context, dir string, env []string, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// GIT_TERMINAL_PROMPT=0 is not cosmetic: without it a clone of a private
	// repository with no usable credential can BLOCK asking for one. A stage
	// pod has no terminal, so the ask would hang until the stage timed out and
	// be reported as a timeout rather than as the auth failure it is.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	return cmd.Run()
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
