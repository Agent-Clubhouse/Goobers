package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
)

// advanceRemoteBranch simulates a concurrent writer (#3366's trigger class 3:
// a push race): after the local worktree forked, another clone pushes a commit
// to the same branch on origin, so the local push is rejected non-fast-forward.
func advanceRemoteBranch(t *testing.T, origin, branch, file, content string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "racer")
	runGitT(t, filepath.Dir(clone), "clone", origin, clone)
	runGitT(t, clone, "config", "user.name", "racer")
	runGitT(t, clone, "config", "user.email", "racer@example.com")
	runGitT(t, clone, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write racer file: %v", err)
	}
	runGitT(t, clone, "add", file)
	runGitT(t, clone, "commit", "-m", "concurrent write")
	runGitT(t, clone, "push", "origin", branch)
}

// TestPushBranchRetriesWithRebaseOnPushRace is #3366's trigger class 3
// regression: site run 93998ce8 finished a fully-validated diff and then died
// on `error: failed to push some refs`. The push layer now fetches the remote
// tip, rebases, and retries instead of discarding minutes of validated work
// over a ref race.
func TestPushBranchRetriesWithRebaseOnPushRace(t *testing.T) {
	origin := initBareOrigin(t)
	const branch = "goobers/implementation/run-race"

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-race", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if err := os.WriteFile(filepath.Join(wt.Path, "local.txt"), []byte("validated implement work\n"), 0o644); err != nil {
		t.Fatalf("write local change: %v", err)
	}
	runGitT(t, wt.Path, "add", "local.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement")

	// The race: a concurrent writer advances the same branch on origin after
	// the local fork point, on a DIFFERENT file so the rebase applies cleanly.
	advanceRemoteBranch(t, origin, branch, "remote.txt", "concurrent remote work\n")

	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "unused-for-local-file-origin")

	code, stdout, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 0 {
		t.Fatalf("push-branch: code = %d, stdout = %q, stderr = %q — the ref race destroyed validated work again (#3366)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "ref race") {
		t.Fatalf("stderr = %q, want a ref-race retry warning", stderr)
	}

	// Both the concurrent commit and the local commit must be on origin.
	lsTree := runGitOutputT(t, wt.Path, "-c", "safe.bareRepository=all", "--git-dir", origin, "ls-tree", "--name-only", "refs/heads/"+branch)
	for _, want := range []string{"local.txt", "remote.txt"} {
		if !strings.Contains(lsTree, want) {
			t.Fatalf("origin branch tree = %q, want both local.txt and remote.txt after rebase-and-retry", lsTree)
		}
	}

	// The successful publication is recorded in the mutation sidecar so the
	// runner journals ref.touched — the signal #3366's re-claim discovery uses
	// to know this run's work was NOT stranded.
	sidecar, err := os.ReadFile(filepath.Join(wt.Path, mutationsSidecarFile))
	if err != nil {
		t.Fatalf("read mutation sidecar: %v", err)
	}
	var fact mutationFact
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(sidecar))), &fact); err != nil {
		t.Fatalf("unmarshal mutation sidecar %q: %v", sidecar, err)
	}
	if fact.Kind != "branch" || fact.ID != branch || fact.Operation != "push" {
		t.Fatalf("sidecar fact = %+v, want a branch/push fact for %s", fact, branch)
	}
}

// TestPushBranchRaceWithConflictSurfacesOriginalError: a rebase that does not
// apply cleanly is agentic work, not a push-layer concern — the rebase aborts
// (leaving the worktree as it was) and the original push rejection surfaces.
func TestPushBranchRaceWithConflictSurfacesOriginalError(t *testing.T) {
	origin := initBareOrigin(t)
	const branch = "goobers/implementation/run-conflict"

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-conflict", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if err := os.WriteFile(filepath.Join(wt.Path, "shared.txt"), []byte("local version\n"), 0o644); err != nil {
		t.Fatalf("write local change: %v", err)
	}
	runGitT(t, wt.Path, "add", "shared.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement")
	localHead := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD"))

	// The concurrent writer touches the SAME file with different content, so
	// the rebase conflicts.
	advanceRemoteBranch(t, origin, branch, "shared.txt", "remote version\n")

	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "unused-for-local-file-origin")

	code, _, stderr := runArgs(t, "push-branch", wt.Path)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (conflicting race is not silently resolvable at the push layer)", code)
	}
	if !strings.Contains(stderr, "failed to push some refs") && !strings.Contains(stderr, "rejected") {
		t.Fatalf("stderr = %q, want the original push rejection surfaced", stderr)
	}
	// The aborted rebase left the worktree exactly as it was.
	if head := strings.TrimSpace(runGitOutputT(t, wt.Path, "rev-parse", "HEAD")); head != localHead {
		t.Fatalf("HEAD = %s, want %s (aborted rebase must restore the branch)", head, localHead)
	}
}

// TestPushBranchRecordsSidecarOnPlainPush: the branch-push mutation fact is
// recorded on the ordinary no-race path too.
func TestPushBranchRecordsSidecarOnPlainPush(t *testing.T) {
	origin := initBareOrigin(t)
	const branch = "goobers/implementation/run-plain"

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-plain", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	runGitT(t, wt.Path, "add", "change.txt")
	runGitT(t, wt.Path, "commit", "-m", "implement")

	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "unused-for-local-file-origin")

	if code, _, stderr := runArgs(t, "push-branch", wt.Path); code != 0 {
		t.Fatalf("push-branch: code = %d, stderr = %q", code, stderr)
	}
	sidecar, err := os.ReadFile(filepath.Join(wt.Path, mutationsSidecarFile))
	if err != nil {
		t.Fatalf("read mutation sidecar: %v", err)
	}
	if !strings.Contains(string(sidecar), `"kind":"branch"`) || !strings.Contains(string(sidecar), `"operation":"push"`) {
		t.Fatalf("sidecar = %q, want a branch push fact", sidecar)
	}
}
