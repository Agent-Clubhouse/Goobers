package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPRSelectDefersBranchOwnedByLiveImplementationRun(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "active", "live", true)
}

func TestPRSelectDefersBranchOwnedInPinnedWorkspace(t *testing.T) {
	testPRSelectBranchOccupancy(t, true, "active", "live", true)
}

func TestPRSelectSelectsBranchWithCleanupPending(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "cleanup-pending", "live", false)
}

func TestPRSelectSelectsBranchKeptForDebugging(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "kept", "live", false)
}

func TestPRSelectSelectsStaleActiveBranchFromSettledRun(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "active", "terminal", false)
}

func testPRSelectBranchOccupancy(t *testing.T, pinned bool, occupancyStatus, ownerState string, wantDeferred bool) {
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
	switch occupancyStatus {
	case "cleanup-pending":
		if err := manager.SetCleanupGuard("blocked", func(context.Context, worktree.CleanupTarget) error {
			return errors.New("handoff blocked")
		}); err != nil {
			t.Fatal(err)
		}
	case "active", "kept":
	default:
		t.Fatalf("unknown occupancy status %q", occupancyStatus)
	}
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: "implementation-run",
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wt.Remove(context.Background(), worktree.RemoveOptions{}) })
	switch occupancyStatus {
	case "cleanup-pending":
		if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); !errors.Is(err, worktree.ErrCleanupDeferred) {
			t.Fatalf("Remove error = %v, want deferred cleanup", err)
		}
	case "kept":
		if err := wt.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
			t.Fatal(err)
		}
	}

	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "implementation-run", Workflow: "implementation",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ownerState == "terminal" {
		if err := run.Append(journal.Event{
			Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
		}); err != nil {
			t.Fatal(err)
		}
	} else if ownerState != "live" {
		t.Fatalf("unknown owner state %q", ownerState)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if wantDeferred {
		if !strings.Contains(stdout, exclusionBranchOccupied) || !strings.Contains(stdout, "implementation-run") {
			t.Fatalf("stdout = %q, want live branch owner exclusion", stdout)
		}
		if !strings.Contains(string(data), `"noWork":true`) || !strings.Contains(string(data), exclusionBranchOccupied) {
			t.Fatalf("result = %s, want no-work with branch occupancy evidence", data)
		}
		return
	}
	var selected map[string]string
	if err := decodePRSelectionTestResult(data, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "5447" {
		t.Fatalf("selected PR = %q, want non-live occupied PR #5447", selected["number"])
	}
}
