//go:build integration

package main

import (
	"context"
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

func TestIntegrationPinnedRecoveryArchivesBeforeNextRunReset(t *testing.T) {
	testdep.Require(t, "git")
	for _, fixture := range []struct {
		name   string
		branch string
	}{
		{name: "main", branch: "main"},
		{name: "master", branch: "master"},
		{name: "slash", branch: "release/2026.09"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			runPinnedRecoveryBaseBranchFixture(t, fixture.branch)
		})
	}
}

func runPinnedRecoveryBaseBranchFixture(t *testing.T, baseBranch string) {
	// A freshly initialized bare mirror may have an unborn master HEAD even
	// though the source uses another branch. Its clone then has a mirror ref
	// but no matching local branch. Recovery must not depend on Git defaults.
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[init]\n\tdefaultBranch = unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch="+baseBranch)
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	ctx := context.Background()
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, manager.Root, func(apiv1.RepoRef) (string, error) { return source, nil }, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	for _, runID := range []string{"pinned-source", "pinned-next"} {
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = run.Close() }()
	}
	lease, err := manager.AcquirePinned(ctx, worktree.PinnedOptions{RepoURL: source, RunID: "pinned-source", BaseRef: baseBranch, Branch: "goobers/implementation/pinned-source"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if got := recoveryCLIGit(t, lease.Worktree.Path, "for-each-ref", "--format=%(refname)", "refs/heads/"+baseBranch); got != "" {
		t.Fatalf("regression fixture unexpectedly has a local base branch: %s", got)
	}
	recoveryCLIGit(t, lease.Worktree.Path, "rev-parse", "--verify", "refs/remotes/mirror/"+baseBranch+"^{commit}")
	// This is the first stage handoff from #4926: it happens before destructive
	// pinned preparation and must use the base recorded in custody.
	if err := lease.Worktree.PreparePinned(ctx, worktree.PinnedPrepareOptions{
		BaseRef: baseBranch, Branch: "goobers/implementation/pinned-source",
	}); err != nil {
		t.Fatalf("prepare first pinned stage from %q: %v", baseBranch, err)
	}
	if err := os.WriteFile(filepath.Join(lease.Worktree.Path, "implementation.txt"), []byte("retained pinned implementation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retained inventory: %v %v", entries, err)
	}
	entry := entries[0]
	if entry.Record.RunID != "pinned-source" {
		t.Fatal("retained wrong run")
	}
	next, err := manager.AcquirePinned(ctx, worktree.PinnedOptions{RepoURL: source, RunID: "pinned-next", BaseRef: baseBranch, Branch: "goobers/implementation/pinned-next", CleanPolicy: worktree.PinnedCleanIgnoredSafe})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Release() }()
	if _, err := os.Stat(filepath.Join(next.Worktree.Path, "implementation.txt")); !os.IsNotExist(err) {
		t.Fatalf("fixture did not reset implementation: %v", err)
	}
	restored := t.TempDir()
	recoveryCLIGit(t, restored, "init", "--initial-branch="+baseBranch)
	if err := recovery.ImportSnapshotBundle(ctx, restored, filepath.Join(filepath.Dir(entry.RecordPath), "snapshot.bundle"), entry.Record, 512<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryCLIGit(t, restored, "show", entry.Record.Ref+":implementation.txt"); got != "retained pinned implementation" {
		t.Fatalf("archive lost implementation: %q", got)
	}
}
