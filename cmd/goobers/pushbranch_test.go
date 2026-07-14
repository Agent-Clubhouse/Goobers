package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
)

// initBareOrigin creates a local bare git repo at dir/origin.git seeded with
// one commit on main, and returns its path — a fake/local "origin" (#237's
// acceptance criteria) so these tests never touch the network.
func initBareOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	seed := filepath.Join(root, "seed")
	runGitT(t, root, "clone", origin, seed)
	runGitT(t, seed, "config", "user.name", "seed")
	runGitT(t, seed, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitT(t, seed, "add", "README.md")
	runGitT(t, seed, "commit", "-m", "seed")
	runGitT(t, seed, "push", "origin", "main")
	return origin
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
}

// branchExistsOnOrigin reports whether branch has a ref in the bare repo at
// originDir — the CLI-level proof that push-branch's push actually reached
// origin, not just the worktree's local git state.
func branchExistsOnOrigin(t *testing.T, originDir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	cmd.Dir = originDir
	return cmd.Run() == nil
}

// TestPushBranchPushesWorktreeBranchToOrigin is #237's core CLI-level
// acceptance: a worktree provisioned exactly the way the real runner does
// (worktree.Manager.Create, over a fake/local bare origin — no network, no
// ambient host git credentials or identity needed since the bare origin
// doesn't authenticate file:// pushes), carrying a commit (standing in for
// the implementer stage), gets its branch pushed to origin by `goobers
// push-branch` — the branch that open-pr's next stage would then find.
func TestPushBranchPushesWorktreeBranchToOrigin(t *testing.T) {
	origin := initBareOrigin(t)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin,
		RunID:   "run-237",
		BaseRef: "main",
		Branch:  "goobers/implementation/run-237",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	// Stand in for the implementer stage: worktree.Manager.Create already set
	// a local bot identity (#237), so this commit needs no ambient git config.
	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	runGitT(t, wt.Path, "add", "change.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement")

	const canaryToken = "canary-repo-push-token-never-on-disk"
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), canaryToken)

	code, stdout, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 0 {
		t.Fatalf("push-branch: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "goobers/implementation/run-237") {
		t.Fatalf("stdout = %q, want a mention of the pushed branch", stdout)
	}
	if !branchExistsOnOrigin(t, origin, "goobers/implementation/run-237") {
		t.Fatal("branch does not exist on origin after push-branch — open-pr would 422 on this")
	}

	// #237 acceptance: the token never lands on disk in the worktree's own
	// git config — it was injected per-invocation via GIT_CONFIG_* env vars,
	// not written with `git config`.
	cmd := exec.Command("git", "config", "--list")
	cmd.Dir = wt.Path
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --list: %v", err)
	}
	if strings.Contains(string(out), canaryToken) {
		t.Fatalf("token leaked into worktree git config: %s", out)
	}
}

// TestPushBranchWorksWithNoAmbientGitIdentity is #237's acceptance criterion
// for a host with no ambient GitHub git credentials and no global git
// identity: HOME points at an empty temp dir (no ~/.gitconfig, no credential
// helper) and GIT_CONFIG_GLOBAL points at a file that doesn't exist. The
// worktree's commit succeeds because worktree.Manager.Create sets a local
// (not --global) bot identity (#237's Create fix), and the push succeeds
// because the credential comes from the runner-injected env var, never from
// a host-level git credential store.
func TestPushBranchWorksWithNoAmbientGitIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "does-not-exist.gitconfig"))

	origin := initBareOrigin(t)
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-noident", BaseRef: "main", Branch: "goobers/implementation/run-noident",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	runGitT(t, wt.Path, "add", "change.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement") // fails if Create didn't set a local identity

	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "unused-for-local-file-origin")

	code, _, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 0 {
		t.Fatalf("push-branch: code = %d, stderr = %q", code, stderr)
	}
	if !branchExistsOnOrigin(t, origin, "goobers/implementation/run-noident") {
		t.Fatal("branch does not exist on origin")
	}
}

// TestPushBranchMissingCredentialFailsClosed proves push-branch refuses to
// run without the repo:push credential the runner is expected to inject —
// no silent, unauthenticated push attempt.
func TestPushBranchMissingCredentialFailsClosed(t *testing.T) {
	origin := initBareOrigin(t)
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-nocred", BaseRef: "main", Branch: "goobers/implementation/run-nocred",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	code, _, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (missing credential)", code)
	}
	if !strings.Contains(stderr, "GOOBERS_CRED_REPO_PUSH") {
		t.Fatalf("stderr = %q, want a mention of the missing credential env var", stderr)
	}
}

// TestPushBranchDetachedHeadFailsClosed proves push-branch refuses to guess
// a branch name for a detached-HEAD worktree (a review/local-ci stage's
// worktree, not an implementer stage's) rather than pushing something wrong.
func TestPushBranchDetachedHeadFailsClosed(t *testing.T) {
	origin := initBareOrigin(t)
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-detached", BaseRef: "main", // no Branch: detached checkout
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "unused")

	code, _, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (detached HEAD)", code)
	}
	if !strings.Contains(stderr, "detached HEAD") {
		t.Fatalf("stderr = %q, want a mention of detached HEAD", stderr)
	}
}
