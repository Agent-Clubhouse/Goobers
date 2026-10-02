package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPRSelectDefersBranchOwnedByLiveImplementationRun(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const (
		number = 5447
		branch = "goobers/implementation/live-run"
	)
	server.addIssue(number, "live branch owner")
	server.addOpenPR(number, branch, "main", "head", "base", false, nil, nil)

	repo := newDaemonFixtureRepo(t)
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return repo, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })

	t.Setenv(executor.GaggleEnvVar, "goobers")
	manager, err := worktree.NewManager(layoutFor(root).ForGaggle("goobers").WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: "implementation-run",
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wt.Remove(context.Background(), worktree.RemoveOptions{}) })

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, exclusionBranchOccupied) || !strings.Contains(stdout, "implementation-run") {
		t.Fatalf("stdout = %q, want live branch owner exclusion", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"noWork":true`) || !strings.Contains(string(data), exclusionBranchOccupied) {
		t.Fatalf("result = %s, want no-work with branch occupancy evidence", data)
	}
}
