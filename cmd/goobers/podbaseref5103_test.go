package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/testgit"
)

// #5103's narrower recurrence, live on a cluster running 299959f2 (#5162):
// every pr-remediation gather-pr-context pod ended with
//
//	fatal: Remote branch goobernetes/pr-remediation/<runID> not found in upstream origin
//	gathered context for PR #5154 (goobernetes/implementation/0517…): behind=false, 1 comment(s)
//	dispatch-exec: recovery custody: recovery base branch "main" is not resolvable in this checkout
//
// — the FALLBACK writable arm (checkoutFallbackBranchAtBase: the derived run
// branch does not exist on the remote yet, so checkout clones base directly
// and creates the run branch locally on top of it), then gather-pr-context's
// own checkoutExistingBranch (gatherprcontext.go) replaces HEAD with the
// selected PR's head branch, exactly as its doc comment says it will
// ("replacing whatever branch the runner's worktree provisioning defaulted
// to"). #5162 only taught ensureRecoveryBaseRemoteRef to run on the
// EXISTING-run-branch arm (finishWritableRepoCheckoutOnExistingBranch); the
// fallback arm was assumed safe because its own clone already leaves a local
// refs/heads/<base> — true at the moment checkout returns, but nothing
// downstream of checkout was ever responsible for keeping it true, and a
// gather-pr-context pod's own git operations run after checkout and before
// recovery custody, outside checkout's control.
//
// This reproduces the exact arm end-to-end: a fresh run's fallback checkout,
// a gather-pr-context-style rebind onto a PR's head branch, base becoming
// unresolvable the way the cluster observed it, and confirms custody now
// resolves it anyway.
func TestRecoveryCustodyResolvesBaseAfterGatherPRContextStyleCheckout(t *testing.T) {
	const prBranch = "goobernetes/implementation/pr5154"
	bare := newBareRepoWithPRBranch(t, "main", prBranch, "")
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "goobernetes/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "pr-remediation")
	t.Setenv(dispatcher.EnvRunID, "run-5154")
	// Deliberately no EnvWorkspaceBranch: gather-pr-context is the stage that
	// DISCOVERS the candidate PR, so nothing has rebound the workspace branch
	// yet — the derived run branch (goobernetes/pr-remediation/run-5154)
	// exists nowhere on the remote, exactly like the cluster's first log line.

	ws := t.TempDir()
	var errOut strings.Builder
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, creds); err != nil {
		t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
	}
	if got := checkedOutBranch(t, ws); got != "goobernetes/pr-remediation/run-5154" {
		t.Fatalf("branch after fallback checkout = %q, want the derived run branch", got)
	}

	// gather-pr-context selects PR #5154 and checks out its head branch
	// directly (gatherprcontext.go's checkoutExistingBranch), replacing HEAD.
	if _, err := checkoutExistingBranch(ws, prBranch, ""); err != nil {
		t.Fatalf("gather-pr-context style checkout of %q: %v", prBranch, err)
	}
	if got := checkedOutBranch(t, ws); got != prBranch {
		t.Fatalf("branch after gather-pr-context checkout = %q, want %q", got, prBranch)
	}

	// Reproduce the cluster's observed state: by the time recovery custody
	// runs, base is unresolvable in this checkout (#5103's narrower
	// recurrence — see this test's package comment for what left it that
	// way on the live cluster; asserted directly here since the whole point
	// of the fix is that custody must not depend on checkout time being the
	// only chance to make base resolvable).
	testgit.Command("-C", ws, "update-ref", "-d", "refs/heads/main").Run()
	testgit.Command("-C", ws, "update-ref", "-d", "refs/remotes/origin/main").Run()
	if _, err := resolveRecoveryBaseRef(context.Background(), ws, "main"); err == nil {
		t.Fatal("test fixture did not actually reproduce base being unresolvable")
	}

	resolved, err := resolveRecoveryBaseRefWithFetch(context.Background(), ws, "main")
	if err != nil {
		t.Fatalf("recovery custody still could not resolve base: %v", err)
	}
	if resolved != "refs/remotes/origin/main" {
		t.Fatalf("resolved base ref = %q, want the re-fetched remote-tracking ref", resolved)
	}
}

// The fallback arm's own clone leaves a local refs/heads/<base>, but #5103
// requires base to resolve the SAME way on every writable arm — a remote-
// tracking ref, not only a local branch a later checkout can displace.
// checkoutFallbackBranchAtBase now calls ensureRecoveryBaseRemoteRef exactly
// as the existing-run-branch arm does (finishWritableRepoCheckoutOnExistingBranch),
// so both arms leave refs/remotes/origin/<base> behind too.
func TestCheckoutFallbackArmAlsoLeavesRemoteTrackingBaseRef(t *testing.T) {
	bare := newBareRepoWithCommit(t, "main")
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "probe")
	t.Setenv(dispatcher.EnvRunID, "run-1")

	ws := t.TempDir()
	var errOut strings.Builder
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, creds); err != nil {
		t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
	}

	resolved, err := resolveRecoveryBaseRef(context.Background(), ws, "main")
	if err != nil {
		t.Fatalf("base unresolvable after fallback checkout: %v", err)
	}
	if resolved != "refs/heads/main" {
		t.Fatalf("resolved base ref = %q, want the local branch the fallback clone leaves", resolved)
	}
	out, err := testgit.Command("-C", ws, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main^{commit}").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Fatalf("fallback arm did not also leave refs/remotes/origin/main resolvable: out=%q err=%v", out, err)
	}
}

// resolveRecoveryBaseRefWithFetch must still fail closed when base truly
// cannot be resolved even after a fetch attempt (no origin remote at all) —
// the fetch is a second chance, not a way to make an unrelated failure look
// like recoverable success.
func TestResolveRecoveryBaseRefWithFetchFailsClosedWithoutAnOrigin(t *testing.T) {
	dir := t.TempDir()
	recoveryPodTestGit(t, dir, "init", "--quiet", "-b", "feature")
	recoveryPodTestGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "feature work")

	if _, err := resolveRecoveryBaseRefWithFetch(context.Background(), dir, "main"); err == nil {
		t.Fatal("expected resolveRecoveryBaseRefWithFetch to fail closed with no origin and no local base")
	}
}
