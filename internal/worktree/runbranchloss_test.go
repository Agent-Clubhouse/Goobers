package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	lostBranchOwner = "run-lost"
	lostBranch      = "goobers/wf/run-lost"
)

// createLostBranchStage provisions one stage of lostBranchOwner on its own
// run branch, the way the runner does for a run's sequential stages.
func createLostBranchStage(t *testing.T, m *Manager, repo, stage string) (*Worktree, error) {
	t.Helper()
	return m.Create(context.Background(), CreateOptions{
		RepoURL: repo, RunID: lostBranchOwner + "-" + stage, OwnerRunID: lostBranchOwner,
		BaseRef: "main", Branch: lostBranch,
	})
}

// implementStage creates the run branch with one commit on it, then tears the
// stage worktree down so the branch lives only in the managed working copy.
func implementStage(t *testing.T, m *Manager, repo string) string {
	t.Helper()
	wt, err := createLostBranchStage(t, m, repo, "implement")
	if err != nil {
		t.Fatalf("create first stage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "impl.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, wt.Path, "add", "-A")
	runTestGit(t, wt.Path, "commit", "-m", "implement")
	if err := wt.Remove(context.Background(), RemoveOptions{}); err != nil {
		t.Fatalf("remove first stage: %v", err)
	}
	repoDir, err := m.WorkingCopy(context.Background(), repo)
	if err != nil {
		t.Fatalf("working copy: %v", err)
	}
	return repoDir
}

func assertRunBranchLost(t *testing.T, m *Manager, repo, repoDir string) {
	t.Helper()
	wt, err := createLostBranchStage(t, m, repo, "review")
	if err == nil {
		_ = wt.Remove(context.Background(), RemoveOptions{})
		t.Fatal("later stage silently recreated a lost run branch")
	}
	if !errors.Is(err, ErrRunBranchLost) {
		t.Fatalf("Create error = %v, want ErrRunBranchLost", err)
	}
	var lost *RunBranchLostError
	if !errors.As(err, &lost) || lost.Branch != lostBranch || lost.OwnerRunID != lostBranchOwner {
		t.Fatalf("Create error = %#v, want RunBranchLostError for %s/%s", err, lostBranchOwner, lostBranch)
	}
	if branchExists(context.Background(), repoDir, lostBranch) {
		t.Fatal("refused Create still recreated the run branch")
	}
}

func TestCreateRefusesRunBranchDeletedBetweenStages(t *testing.T) {
	repo := newSourceRepo(t)
	m := newTestManager(t)
	repoDir := implementStage(t, m, repo)

	runTestGit(t, repoDir, "branch", "-D", lostBranch)

	assertRunBranchLost(t, m, repo, repoDir)
}

// A pruning fetch over the shared mirror (the unpinned-to-pinned migration
// hazard, #6433) deletes a local-only run branch the same way.
func TestCreateRefusesRunBranchPrunedByMirrorFetch(t *testing.T) {
	repo := newSourceRepo(t)
	m := newTestManager(t)
	repoDir := implementStage(t, m, repo)

	runTestGit(t, repoDir, "fetch", "--prune", "origin", "+refs/heads/*:refs/heads/*")
	if branchExists(context.Background(), repoDir, lostBranch) {
		t.Fatal("test setup: pruning fetch kept the local-only run branch")
	}

	assertRunBranchLost(t, m, repo, repoDir)
}

func TestCreateKeepsSequentialStagesAndFirstStageUnaffected(t *testing.T) {
	repo := newSourceRepo(t)
	m := newTestManager(t)
	ctx := context.Background()

	// A first-stage retry after a failed attempt left no branch behind still
	// creates it: there is no record until a Create got the branch in place.
	implementStage(t, m, repo)

	review, err := createLostBranchStage(t, m, repo, "review")
	if err != nil {
		t.Fatalf("create second stage on intact branch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(review.Path, "impl.txt")); err != nil || string(got) != "work\n" {
		t.Fatalf("second stage impl.txt = %q, %v; want the first stage's commit", got, err)
	}
	if err := review.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	// Another run cutting its own branch is never refused by this run's record.
	other, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-other-implement", OwnerRunID: "run-other",
		BaseRef: "main", Branch: "goobers/wf/run-other",
	})
	if err != nil {
		t.Fatalf("create other run's first stage: %v", err)
	}
	if err := other.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	key := repoKey(repo)
	if _, err := m.FinalizeRun(ctx, lostBranchOwner); err != nil {
		t.Fatalf("finalize run: %v", err)
	}
	if _, err := os.Stat(m.runBranchEstablishedPath(key, lostBranchOwner, lostBranch)); !os.IsNotExist(err) {
		t.Fatalf("run branch record survived FinalizeRun: %v", err)
	}
}

// The rebound-branch path (RequireExistingBranch) keeps its own refusal and
// never writes a run-branch record.
func TestCreateRequireExistingBranchUnchangedByRunBranchRecord(t *testing.T) {
	repo := newSourceRepo(t)
	m := newTestManager(t)
	ctx := context.Background()
	const branch = "goobers/wf/rebound"
	runTestGit(t, repo, "branch", branch)

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-rebound-a", OwnerRunID: "run-rebound",
		BaseRef: "main", Branch: branch, RequireExistingBranch: true, AcquireRemoteBranch: true,
	})
	if err != nil {
		t.Fatalf("create rebound stage: %v", err)
	}
	if _, err := os.Stat(m.runBranchEstablishedPath(wt.key, "run-rebound", branch)); !os.IsNotExist(err) {
		t.Fatalf("rebound Create wrote a run-branch record: %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-missing-a", OwnerRunID: "run-missing",
		BaseRef: "main", Branch: "goobers/wf/never-existed", RequireExistingBranch: true,
	})
	if err == nil || errors.Is(err, ErrRunBranchLost) || !strings.Contains(err.Error(), "refusing to create it") {
		t.Fatalf("missing rebound branch error = %v, want the existing RequireExistingBranch refusal", err)
	}
}
