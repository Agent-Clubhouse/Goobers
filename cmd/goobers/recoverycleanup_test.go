package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestRecoveryCleanupUsesDurableBaseRef(t *testing.T) {
	for _, baseRef := range []string{
		"refs/heads/main",
		"refs/heads/master",
		"refs/heads/release/2026.09",
		"refs/remotes/mirror/main",
		"refs/remotes/mirror/master",
		"refs/remotes/mirror/release/2026.09",
		"refs/tags/v1.0.0",
		"0123456789012345678901234567890123456789",
	} {
		t.Run(strings.ReplaceAll(baseRef, "/", "_"), func(t *testing.T) {
			got, err := recoveryCleanupBaseRef(worktree.CleanupTarget{BaseRef: baseRef})
			if err != nil || got != baseRef {
				t.Fatalf("recoveryCleanupBaseRef() = %q, %v; want %q", got, err, baseRef)
			}
		})
	}
}

func TestRecoveryCleanupRefusesMissingDurableBaseRef(t *testing.T) {
	if _, err := recoveryCleanupBaseRef(worktree.CleanupTarget{}); err == nil {
		t.Fatal("missing owning-run base reference permitted recovery cleanup")
	}
}

func TestRecoveryCleanupTerminalNoWorkTargetWithoutHEAD(t *testing.T) {
	testdep.Require(t, "git")

	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	cmd := testgit.Command("init", "--initial-branch=main")
	cmd.Dir = repository
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	workcopies := t.TempDir()
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "cleanup-headless-nowork"
	startedAt := time.Now().UTC().Add(-time.Hour)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}).CanonicalKey()
	target := worktree.CleanupTarget{
		Path: repository, WorktreeID: runID + "-stage", OwnerRunID: runID,
		BaseRef: "refs/heads/main", CreatedAt: startedAt,
	}
	if err := recoveryCleanupCurrentTarget(context.Background(), layout, cfg, workcopies, journal.NewRegistryScrubber(), manager, key, target); err != nil {
		t.Fatalf("terminal no-work cleanup attempted recovery capture without HEAD: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("no-work cleanup created %d recovery entries, want none", len(entries))
	}
}

func TestRecoveryCleanupRunningNoWorkTargetWithoutHEAD(t *testing.T) {
	testdep.Require(t, "git")

	for _, tc := range []struct {
		name    string
		dirty   bool
		wantErr bool
	}{
		{name: "empty"},
		{name: "untracked", dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			repository := t.TempDir()
			runTestGit(t, repository, "init", "--initial-branch=main")
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(repository, "evidence.txt"), []byte("preserve me"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			workcopies := t.TempDir()
			manager, err := worktree.NewManager(workcopies)
			if err != nil {
				t.Fatal(err)
			}
			const runID = "cleanup-headless-running"
			startedAt := time.Now().UTC().Add(-time.Hour)
			run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
				Schema: journal.RunSchema, RunID: runID, Workflow: "merge-review",
				WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}

			key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}).CanonicalKey()
			target := worktree.CleanupTarget{
				Path: repository, WorktreeID: runID + "-stage", OwnerRunID: runID,
				BaseRef: "refs/heads/main", CreatedAt: startedAt,
			}
			err = recoveryCleanupCurrentTarget(context.Background(), layout, cfg, workcopies, journal.NewRegistryScrubber(), manager, key, target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("running-run headless cleanup error = %v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestRecoveryCleanupRunningDirtyStageDoesNotRetainCurrentWorktree(t *testing.T) {
	testdep.Require(t, "git")

	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	repository := createRecoveryCleanupSource(t)
	if err := os.WriteFile(filepath.Join(repository, "stage-output.txt"), []byte("ordinary stage output"), 0o600); err != nil {
		t.Fatal(err)
	}
	workcopies := t.TempDir()
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "cleanup-dirty-running-stage"
	startedAt := time.Now().UTC().Add(-time.Hour)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}).CanonicalKey()
	target := worktree.CleanupTarget{
		Path: repository, WorktreeID: runID + "-stage", OwnerRunID: runID,
		BaseRef: "refs/heads/main", CreatedAt: startedAt,
	}
	if err := recoveryCleanupCurrentTarget(context.Background(), layout, cfg, workcopies, journal.NewRegistryScrubber(), manager, key, target); err != nil {
		t.Fatalf("running dirty stage cleanup retained current worktree: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("running dirty stage cleanup created %d recovery entries, want none", len(entries))
	}
	assertNoRecoveryHandoffEvent(t, layout, runID)
}

func TestRecoveryCleanupTerminalRepositorylessTarget(t *testing.T) {
	testdep.Require(t, "git")

	for _, tc := range []struct {
		name      string
		writeFile string
		wantErr   bool
	}{
		{name: "empty"},
		{name: "disposable-result", writeFile: "claimed-item.json"},
		{name: "unknown-result", writeFile: "analysis-result.json", wantErr: true},
		{name: "recoverable", writeFile: "evidence.txt", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			repository := t.TempDir()
			workcopies := t.TempDir()
			manager, err := worktree.NewManager(workcopies)
			if err != nil {
				t.Fatal(err)
			}
			const runID = "cleanup-repositoryless-nowork"
			startedAt := time.Now().UTC().Add(-time.Hour)
			createFinishedRun(t, layout, runID, startedAt)
			if tc.writeFile != "" {
				if err := os.WriteFile(filepath.Join(repository, tc.writeFile), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}).CanonicalKey()
			target := worktree.CleanupTarget{
				Path: repository, WorktreeID: runID + "-stage", OwnerRunID: runID,
				BaseRef: "refs/heads/main", CreatedAt: startedAt,
			}
			err = recoveryCleanupCurrentTarget(context.Background(), layout, cfg, workcopies, journal.NewRegistryScrubber(), manager, key, target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("repository-less cleanup error = %v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestRecoveryCleanupTerminalFinalizationNoWorkTargets(t *testing.T) {
	testdep.Require(t, "git")

	for _, tc := range []struct {
		name       string
		repoLess   bool
		dirtyFile  string
		disposable bool
		wantErr    bool
	}{
		{name: "unborn-repository"},
		{name: "repositoryless-empty", repoLess: true},
		{name: "repositoryless-disposable-artifact", repoLess: true, disposable: true},
		{name: "repositoryless-unknown-result", repoLess: true, dirtyFile: "analysis-result.json", wantErr: true},
		{name: "unborn-dirty", dirtyFile: "evidence.txt", wantErr: true},
		{name: "repositoryless-recoverable", repoLess: true, dirtyFile: "evidence.txt", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			source, workcopies := createRecoveryCleanupSource(t), t.TempDir()
			previousCloneURL := repoCloneURL
			repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
			t.Cleanup(func() { repoCloneURL = previousCloneURL })
			manager, err := worktree.NewManager(workcopies)
			if err != nil {
				t.Fatal(err)
			}
			option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
			if err != nil {
				t.Fatal(err)
			}
			option(manager)

			runID := "cleanup-finalize-" + strings.ReplaceAll(tc.name, "-", "_")
			startedAt := time.Now().UTC().Add(-time.Hour)
			createFinishedRun(t, layout, runID, startedAt)
			workspace, err := manager.Create(context.Background(), worktree.CreateOptions{
				RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID,
				BaseRef: "main", Branch: "goobers/implementation/" + runID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.repoLess {
				makeWorkspaceRepositoryless(t, manager, source, workspace.Path)
				if tc.disposable {
					if err := os.WriteFile(filepath.Join(workspace.Path, "claimed-item.json"), []byte("{}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				makeWorkspaceUnborn(t, workspace.Path)
			}
			if tc.dirtyFile != "" {
				if err := os.WriteFile(filepath.Join(workspace.Path, tc.dirtyFile), []byte("preserve me"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err = finalizeTerminalRunWithClaimRelease(layout, nil, manager, runID, func(instance.Layout, *journal.InstanceLog, string) error { return nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("terminal finalization error = %v, wantErr=%t", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, worktree.ErrCleanupDeferred) {
					t.Fatalf("dirty/recoverable cleanup error = %v, want cleanup deferred", err)
				}
				if _, statErr := os.Stat(workspace.Path); statErr != nil {
					t.Fatalf("recoverable target was not preserved: %v", statErr)
				}
				return
			}
			if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("no-work target survived cleanup: %v", err)
			}
			assertNoWorktreeRemoveFailed(t, layout, runID)
			assertNoRecoveryHandoffEvent(t, layout, runID)
		})
	}
}

func createFinishedRun(t *testing.T, layout instance.Layout, runID string, startedAt time.Time) {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "merge-review",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func createRecoveryCleanupSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	runTestGit(t, source, "init", "--initial-branch=main")
	runTestGit(t, source, "config", "user.email", "test@example.invalid")
	runTestGit(t, source, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "README.md")
	runTestGit(t, source, "commit", "-m", "base")
	return source
}

func makeWorkspaceUnborn(t *testing.T, path string) {
	t.Helper()
	runTestGit(t, path, "checkout", "--orphan", "empty-nowork")
	runTestGit(t, path, "rm", "-rf", "--quiet", ".")
}

func makeWorkspaceRepositoryless(t *testing.T, manager *worktree.Manager, repoURL, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(path, ".git")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "README.md")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	found, err := manager.WithExistingMirror(context.Background(), repoURL, func(repository string) error {
		cmd := testgit.Command("-c", "safe.bareRepository=all", "-C", repository, "worktree", "prune")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git worktree prune: %w: %s", err, output)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("prune repository-less worktree registration: found=%t err=%v", found, err)
	}
}

func assertNoWorktreeRemoveFailed(t *testing.T, layout instance.Layout, runID string) {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "worktree_remove_failed" {
			t.Fatalf("unexpected worktree_remove_failed event: %+v", event.Error)
		}
	}
}

func assertNoRecoveryHandoffEvent(t *testing.T, layout instance.Layout, runID string) {
	t.Helper()
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Runner["recoveryCapture"] == true {
			t.Fatalf("unexpected recovery handoff event: %+v", event.Runner)
		}
	}
}
