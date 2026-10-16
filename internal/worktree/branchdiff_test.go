package worktree

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// TestRunBranchDiffMatchesWorktreeDiff is #5414: a reviewer that is not on
// the run branch reads the run's committed diff from the shared mirror, and
// it must be byte-identical to what a worktree on that branch reports. A
// branch or mirror that does not exist carries no change.
func TestRunBranchDiffMatchesWorktreeDiff(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	if got, err := m.RunBranchDiff(ctx, repo, "main", "goobers/impl/run-5414"); err != nil || got != nil {
		t.Fatalf("RunBranchDiff before any mirror = %q, %v; want nil, nil", got, err)
	}

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-5414", BaseRef: "main", Branch: "goobers/impl/run-5414",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustWriteFile(t, filepath.Join(wt.Path, "feature.go"), "package feature\n\nfunc Added() {}\n")
	runTestGit(t, wt.Path, "add", "-A")
	runTestGit(t, wt.Path, "commit", "-m", "add feature")

	want, err := wt.Diff(ctx, "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("worktree diff is empty after a commit")
	}
	got, err := m.RunBranchDiff(ctx, repo, "main", "goobers/impl/run-5414")
	if err != nil {
		t.Fatalf("RunBranchDiff: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("RunBranchDiff differs from Worktree.Diff:\n got: %s\nwant: %s", got, want)
	}

	if got, err := m.RunBranchDiff(ctx, repo, "main", "goobers/impl/never-created"); err != nil || got != nil {
		t.Fatalf("RunBranchDiff for a missing branch = %q, %v; want nil, nil", got, err)
	}
}
