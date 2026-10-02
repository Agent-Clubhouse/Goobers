package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPRSelectDefersBranchOwnedByLiveImplementationRun(t *testing.T) {
	testPRSelectDefersBranchOwnedByLiveImplementationRun(t, false)
}

func TestPRSelectDefersBranchOwnedInPinnedWorkspace(t *testing.T) {
	testPRSelectDefersBranchOwnedByLiveImplementationRun(t, true)
}

func testPRSelectDefersBranchOwnedByLiveImplementationRun(t *testing.T, pinned bool) {
	t.Helper()
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

	layout := layoutFor(root)
	if pinned {
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Repos) != 1 {
			t.Fatalf("configured repos = %d, want 1", len(cfg.Repos))
		}
		cfg.Repos[0].Workspace = &instance.RepoWorkspaceConfig{Pinned: true}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(layout.ConfigFile(), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv(executor.GaggleEnvVar, "goobers")
	scoped := layout.ForGaggle("goobers")
	workcopiesRoot := scoped.WorkcopiesDir()
	if pinned {
		workcopiesRoot = scoped.WorkcopiesBaseDir()
	}
	manager, err := worktree.NewManager(workcopiesRoot)
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
