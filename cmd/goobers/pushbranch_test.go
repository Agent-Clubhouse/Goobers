package main

import (
	"encoding/base64"
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
	runGitOutputT(t, dir, args...)
}

func runGitOutputT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
	return string(out)
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
	// not written with `git config`. Checks both the raw config FILE bytes
	// (not just `git config --list`'s resolved view, which a future change
	// piping credentials through some other mechanism might not surface the
	// same way) and every shape the credential takes on the wire: the raw
	// token, its base64(x-access-token:token) form (what actually crosses
	// the wire as the header value), and the header name/scheme literals —
	// a regression that persisted the auth via `git config
	// http.extraheader` instead of the env-var injection would leak a
	// reversible credential to disk without ever containing the raw token
	// string.
	gitCommonDir := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(wt.Path, gitCommonDir)
	}
	configBytes, err := os.ReadFile(filepath.Join(gitCommonDir, "config"))
	if err != nil {
		t.Fatalf("read git config file: %v", err)
	}
	authHeaderValue := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + canaryToken))
	for _, leak := range []string{canaryToken, authHeaderValue, "extraheader", "AUTHORIZATION"} {
		if strings.Contains(string(configBytes), leak) {
			t.Fatalf("credential material (%q) leaked into git config file %s: %s", leak, filepath.Join(gitCommonDir, "config"), configBytes)
		}
	}
}

// TestPushBranchWorksWithNoAmbientGitIdentity is #237's acceptance criterion
// for a host with no ambient GitHub git credentials and no global git
// identity: HOME points at an empty temp dir, GIT_CONFIG_SYSTEM points at
// /dev/null (no /etc/gitconfig fallback), and GIT_CONFIG_GLOBAL sets
// user.useConfigOnly=true — the setting that makes git refuse to
// auto-derive an identity from the OS user/hostname (git >= 2.50 does this
// by default when no identity is configured anywhere, which would let this
// test pass even if worktree.Manager.Create set no local identity at all;
// useConfigOnly closes that gap so a missing Create fix fails the commit
// here, not just in TestManager_Create_SetsLocalBotIdentity's direct
// config-value assertion). The worktree's commit succeeds because Create
// sets a local (not --global) bot identity, and the push succeeds because
// the credential comes from the runner-injected env var, never from a
// host-level git credential store.
func TestPushBranchWorksWithNoAmbientGitIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	globalConfig := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

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
