//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryCleanupArchivesBeforeRemovingActiveRunWorktree(t *testing.T) {
	testdep.Require(t, "git")
	for _, terminal := range []bool{false, true} {
		name := "stage"
		if terminal {
			name = "standalone-terminal"
		}
		t.Run(name, func(t *testing.T) { runRecoveryCleanupFixture(t, terminal, false, false, false, "main") })
	}
	t.Run("terminal-after-stage-removal", func(t *testing.T) { runRecoveryCleanupFixture(t, true, true, false, false, "main") })
	t.Run("abandoned-preparation-stage", func(t *testing.T) { runRecoveryCleanupFixture(t, false, false, true, false, "main") })
	t.Run("abandoned-preparation-terminal", func(t *testing.T) { runRecoveryCleanupFixture(t, true, false, true, false, "main") })
	t.Run("abandoned-preparation-missing-record", func(t *testing.T) { runRecoveryCleanupFixture(t, false, false, true, true, "main") })
	t.Run("master-base", func(t *testing.T) { runRecoveryCleanupFixture(t, false, false, false, false, "master") })
	t.Run("slash-base", func(t *testing.T) { runRecoveryCleanupFixture(t, false, false, false, false, "release/2026.09") })
}

func runRecoveryCleanupFixture(t *testing.T, terminal, removeBeforeTerminal, abandoned, interruptRecord bool, baseBranch string) {
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })
	recoveryCLIGit(t, source, "init", "--initial-branch="+baseBranch)
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	const runID = "cleanup-recovery"
	startedAt := time.Now().UTC()
	if terminal {
		startedAt = startedAt.Add(-45 * 24 * time.Hour)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the run's exclusive writer open throughout cleanup. Recovery may
	// read identity but must never try to reopen that writer.
	defer func() { _ = run.Close() }()
	option, err := recoveryCleanupOption(layout, cfg, workcopies, func(apiv1.RepoRef) (string, error) { return source, nil }, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	option(manager) // A configuration reload must replace, not duplicate.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: baseBranch, Branch: "goobers/implementation/" + runID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "implementation.txt"), []byte("recover me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if abandoned {
		prepared, err := recovery.PreparedRestoreBranch(runID)
		if err != nil {
			t.Fatal(err)
		}
		recoveryCLIGit(t, workspace.Path, "checkout", "-b", prepared)
		recoveryCLIGit(t, workspace.Path, "add", "implementation.txt")
		recoveryCLIGit(t, workspace.Path, "commit", "-m", "Restore retained implementation for prior-run")
		recoveryCLIGit(t, workspace.Path, "checkout", "goobers/implementation/"+runID)
	}
	if interruptRecord {
		root, err := prepareRecoveryInventory(layout.Root)
		if err != nil {
			t.Fatal(err)
		}
		identities, err := recoveryRepositoryIdentities(cfg, func(apiv1.RepoRef) (string, error) { return source, nil })
		if err != nil {
			t.Fatal(err)
		}
		recoveryCfg := cfg.Retention.RecoveryEffective()
		retainWindow, err := recoveryCfg.RetainWindowEffective()
		if err != nil {
			t.Fatal(err)
		}
		interrupted := errors.New("interrupt after retained publication")
		request := recovery.RetentionRequest{
			Repository: workspace.Path, RepositoryKey: identities[worktree.RepositoryDigest(source)], RunID: runID,
			BaseRef: baseBranch, IdentityTime: startedAt, RetainUntil: startedAt.Add(retainWindow),
			InventoryRoot: root, CleanupRoots: []string{workcopies},
			MaxSnapshots: recoveryCfg.MaxSnapshotsEffective(), MaxArchiveBytes: recoveryCfg.MaxArchiveBytesEffective(),
		}
		if err := recovery.RetainAbandonedPreparation(ctx, request, recoveryCleanupTestJournal{err: interrupted}); !errors.Is(err, interrupted) {
			t.Fatalf("seed interrupted recovery publication: %v", err)
		}
		entries, err := recovery.ReadInventory(ctx, root, recoveryCfg.MaxSnapshotsEffective())
		if err != nil || len(entries) != 1 {
			t.Fatalf("inspect interrupted recovery publication: %+v %v", entries, err)
		}
		if err := os.Remove(entries[0].RecordPath); err != nil {
			t.Fatal(err)
		}
	}
	if terminal {
		if removeBeforeTerminal {
			if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("fixture must renew without any worktree: %v", err)
			}
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
		// Mimic a standalone startup/abort finalizer that did not construct
		// the worktree and has never gone through buildRunnerConfig.
		standalone, createErr := worktree.NewManager(workcopies)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if !removeBeforeTerminal && !abandoned {
			assertTerminalRecoveryConfigFailurePreservesWorktree(t, layout, standalone, runID, workspace.Path)
		}
		err = finalizeTerminalRunWithClaimRelease(layout, nil, standalone, runID, func(instance.Layout, *journal.InstanceLog, string) error { return nil })
	} else {
		err = workspace.Remove(ctx, worktree.RemoveOptions{})
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged cleanup did not remove worktree: %v", err)
	}
	if interruptRecord {
		retry, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: baseBranch, Branch: "goobers/implementation/" + runID})
		if err != nil {
			t.Fatalf("repaired cleanup left a self-colliding worktree branch: %v", err)
		}
		if err := retry.Remove(ctx, worktree.RemoveOptions{}); err != nil {
			t.Fatalf("remove replacement worktree: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	var retained recovery.Record
	for _, entry := range entries {
		if entry.IsDir() {
			retained, err = recovery.ReadRetainedRecord(filepath.Join(layout.Root, "recovery", entry.Name(), recovery.RecordFileName))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if retained.RunID != runID {
		t.Fatal("cleanup removed worktree without a bound recovery record")
	}
	if terminal && !retained.RetainUntil.After(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("long-running terminal job lost its recovery window: started=%s deadline=%s", startedAt, retained.RetainUntil)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	for _, event := range events {
		if event.RunID == runID && event.Runner["recoveryRef"] == retained.Ref {
			observations++
		}
	}
	wantObservations := 1
	if removeBeforeTerminal {
		wantObservations = 2 // initial capture and terminal renewal
	}
	if observations != wantObservations {
		t.Fatalf("cleanup requires %d recovery observations, got %d", wantObservations, observations)
	}
	if abandoned {
		found, err := manager.WithExistingMirror(ctx, source, func(repository string) error {
			prepared, err := recovery.PreparedRestoreBranch(runID)
			if err != nil {
				return err
			}
			if refs := recoveryCLIGit(t, repository, "for-each-ref", "--format=%(refname)", "refs/heads/"+prepared); refs != "" {
				t.Fatalf("abandoned preparation survived cleanup: %s", refs)
			}
			if got := recoveryCLIGit(t, repository, "show", retained.Ref+":implementation.txt"); got != "recover me" {
				t.Fatalf("prepared implementation was lost: %q", got)
			}
			return nil
		})
		if err != nil || !found {
			t.Fatalf("verify retained preparation: %t %v", found, err)
		}
	}
}

type recoveryCleanupTestJournal struct{ err error }

func (l recoveryCleanupTestJournal) Append(journal.Event) error { return l.err }

func assertTerminalRecoveryConfigFailurePreservesWorktree(t *testing.T, layout instance.Layout, manager *worktree.Manager, runID, path string) {
	t.Helper()
	config, unavailable := layout.ConfigFile(), layout.ConfigFile()+".unavailable"
	if err := os.Rename(config, unavailable); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(unavailable, config); err != nil {
			t.Error(err)
		}
	}()
	before := recoveryCLIGit(t, path, "rev-parse", "HEAD")
	err := finalizeTerminalRunWithClaimRelease(layout, nil, manager, runID, func(instance.Layout, *journal.InstanceLog, string) error { return nil })
	if !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("unavailable configuration did not defer destructive cleanup: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "implementation.txt")); err != nil || string(data) != "recover me" {
		t.Fatalf("failed terminal handoff lost implementation: %q %v", data, err)
	}
	if after := recoveryCLIGit(t, path, "rev-parse", "HEAD"); after != before {
		t.Fatal("failed terminal handoff changed HEAD")
	}
}
