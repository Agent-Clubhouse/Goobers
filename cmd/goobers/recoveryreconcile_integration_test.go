//go:build integration

package main

import (
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

// One incomplete reservation used to fail EVERY later worktree cleanup on the
// instance: the abandoned-preparation handoff read the inventory strictly, so
// a directory holding only `.publish.lock` and `snapshot.bundle.lock` — debris
// from some unrelated run's crashed publish — made cleanup fail with "inspect
// recovery reservation <hash>: inspect recovery record: ... record.json", after
// every stage, until an operator deleted files by hand (#5177 AC3, #5354).
//
// The debris here is younger than the reclamation grace window, so the cleanup
// cannot pass by deleting it: it has to succeed with the debris still in place.
func TestIntegrationRecoveryCleanupSucceedsBesideIncompleteReservations(t *testing.T) {
	testdep.Require(t, "git")
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")

	const runID = "debris-cleanup"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().UTC().Add(-time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, func(apiv1.RepoRef) (string, error) {
		return source, nil
	}, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	workspace, err := manager.Create(t.Context(), worktree.CreateOptions{
		RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID,
		BaseRef: "main", Branch: "goobers/implementation/" + runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAbandonedPreparation(t, workspace.Path, runID)

	root := filepath.Join(layout.Root, "recovery")
	debris := seedIncompleteRecoveryReservation(t, root, 1, 0)

	if err := workspace.Remove(t.Context(), worktree.RemoveOptions{}); err != nil {
		t.Fatalf("unrelated cleanup failed beside an incomplete reservation: %v", err)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("cleanup raced a reservation that may still be publishing: %v", err)
	}
	entries, unreadable, err := recovery.ReadInventoryTolerant(t.Context(), root, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("cleanup published no durable recovery handoff")
	}
	if len(unreadable) != 1 || filepath.Join(root, unreadable[0].Name) != debris {
		t.Fatalf("debris was not reported as unreadable: %+v", unreadable)
	}
}

// seedAbandonedPreparation leaves the exact prepared-restore branch an
// interrupted preparation leaves behind, which is what makes cleanup reach the
// abandoned-preparation handoff and its inventory read.
func seedAbandonedPreparation(t *testing.T, repository, runID string) {
	t.Helper()
	branch, err := recovery.PreparedRestoreBranch(runID)
	if err != nil {
		t.Fatal(err)
	}
	current := recoveryCLIGit(t, repository, "rev-parse", "--abbrev-ref", "HEAD")
	recoveryCLIGit(t, repository, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "prepared.txt"), []byte("prepared work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repository, "add", "prepared.txt")
	recoveryCLIGit(t, repository, "commit", "-m", "prepared")
	recoveryCLIGit(t, repository, "checkout", current)
}
