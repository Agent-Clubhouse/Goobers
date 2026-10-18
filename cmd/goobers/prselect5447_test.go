package main

import (
	"context"
	"encoding/json"
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
	testPRSelectBranchOccupancy(t, false, "active", "live", false)
}

func TestPRSelectDefersBranchOwnedInPinnedWorkspace(t *testing.T) {
	testPRSelectBranchOccupancy(t, true, "active", "live", false)
}

func TestPRSelectDefersBranchWithCleanupPending(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "cleanup-pending", "live", false)
}

func TestPRSelectDefersBranchKeptForDebugging(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "kept", "live", false)
}

func TestPRSelectDefersStaleActiveBranchFromSettledRun(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "active", "terminal", false)
}

func TestPRSelectDefersActiveRunningOccupancyWithDeadProcess(t *testing.T) {
	testPRSelectBranchOccupancy(t, false, "active", "live", true)
}

func testPRSelectBranchOccupancy(t *testing.T, pinned bool, occupancyStatus, ownerState string, deadOwnerProcess bool) {
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
	if deadOwnerProcess {
		setWorktreeOwnerPID(t, workcopiesRoot, "implementation-run", 999999)
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

	occupancies, err := manager.BranchOccupancies(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	occupancy, occupied := occupancies[branch]
	if !occupied {
		t.Fatal("fixture branch is not registered before selection")
	}
	if deadOwnerProcess && occupancy.OwnerProcessLive {
		t.Fatalf("occupancy = %+v, want dead owner process", occupancy)
	}
	_, err = manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: "merge-review-stage", OwnerRunID: "merge-review-run",
		BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), "owned by another run") {
		t.Fatalf("cross-run acquisition error = %v, want ownership refusal", err)
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
	if !strings.Contains(stdout, exclusionBranchOccupied) || !strings.Contains(stdout, "implementation-run") {
		t.Fatalf("stdout = %q, want registered branch exclusion", stdout)
	}
	if !strings.Contains(string(data), `"noWork":true`) || !strings.Contains(string(data), exclusionBranchOccupied) {
		t.Fatalf("result = %s, want no-work with branch occupancy evidence", data)
	}
}

func setWorktreeOwnerPID(t *testing.T, root, ownerRunID string, pid int) {
	t.Helper()
	updated := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record map[string]any
		if json.Unmarshal(data, &record) != nil || record["owner_run_id"] != ownerRunID || record["pid"] == nil {
			return nil
		}
		record["pid"] = pid
		data, err = json.Marshal(record)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		updated++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 2 {
		t.Fatalf("updated %d ownership records, want 2", updated)
	}
}
