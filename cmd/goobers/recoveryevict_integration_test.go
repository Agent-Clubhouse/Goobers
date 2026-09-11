//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationRecoveryCleanupEvictsLandedEntryUnderFullInventory is #4823's
// core regression fixture: today ErrInventoryFull is asserted only at the
// single-reservation level (TestCheckSupportMatrixForRelease-style unit
// coverage never drives a second, competing entry through the same
// inventory). This drives a full run lifecycle against an inventory already
// at its configured cap (MaxSnapshots+1 cleanups total) and asserts the LAST
// run still finalizes cleanly — the fix retires a terminal, already-landed
// entry to make room, rather than refusing and leaving the new worktree
// undeletable (the wedge #4823 reports).
func TestIntegrationRecoveryCleanupEvictsLandedEntryUnderFullInventory(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryCLIGit(t, source, "rev-parse", "HEAD")

	workcopies := filepath.Join(layout.Root, "workcopies")
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}

	cloneURL := func(apiv1.RepoRef) (string, error) { return source, nil }
	previous := repoCloneURL
	repoCloneURL = cloneURL
	t.Cleanup(func() { repoCloneURL = previous })

	// Cap the inventory at exactly 2: one slot for a run still in flight
	// (must survive eviction untouched) and one for a terminal, landed run
	// (must be retired to make room for the "+1"th cleanup).
	cfg := &instance.Config{
		Repos:     []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}},
		Retention: instance.RetentionConfig{Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: 2}},
	}
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}).CanonicalKey()
	root, err := prepareRecoveryInventory(layout.Root)
	if err != nil {
		t.Fatal(err)
	}

	// Slot 1: a run still in progress. Must never be evicted.
	const runningRunID = "running-run"
	recoveryCLIGit(t, source, "checkout", "-b", "running-branch")
	if err := os.WriteFile(filepath.Join(source, "running.txt"), []byte("still working"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "in-flight implementation")
	runningSnapshot := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	recoveryCLIGit(t, source, "checkout", "main")

	// Slot 2: a terminal run whose implementation has already landed on main.
	const landedRunID = "landed-run"
	recoveryCLIGit(t, source, "checkout", "-b", "landed-branch")
	if err := os.WriteFile(filepath.Join(source, "landed.txt"), []byte("landed work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "landed implementation")
	landedSnapshot := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	recoveryCLIGit(t, source, "checkout", "main")

	// Recovery refs/objects live in the manager's own managed mirror, not in
	// source directly (WithRecoveryRepositories/Locked only ever look at a
	// managed mirror or pinned clone) — create it now that every commit it
	// must contain already exists in source, matching how the existing
	// periodic-sweep fixture (testRecoveryExpirySweep) orders this.
	mirror, err := manager.WorkingCopy(ctx, source)
	if err != nil {
		t.Fatal(err)
	}

	runningRun, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runningRunID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().Add(-time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runningRun.Close() }()
	runningRef, err := recovery.RefForSnapshot(runningRunID, runningSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	runningDigest, err := recovery.WriteSnapshotPatch(ctx, mirror, base, runningSnapshot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	runningRecord := recovery.Record{
		Version: 1, RunID: runningRunID, RepositoryKey: key, Ref: runningRef,
		BaseSHA: base, SnapshotSHA: runningSnapshot, PatchDigest: runningDigest,
		CreatedAt: time.Now().Add(-time.Hour), RetainUntil: time.Now().Add(29 * 24 * time.Hour),
	}
	if _, _, err := recovery.PublishToInventory(ctx, mirror, root, []string{manager.Root}, runningRecord, 2, 1<<20); err != nil {
		t.Fatal(err)
	}

	landedRun, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: landedRunID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().Add(-2 * time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := landedRun.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := landedRun.Close(); err != nil {
		t.Fatal(err)
	}
	landedRef, err := recovery.RefForSnapshot(landedRunID, landedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	landedDigest, err := recovery.WriteSnapshotPatch(ctx, mirror, base, landedSnapshot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	landedRecord := recovery.Record{
		Version: 1, RunID: landedRunID, RepositoryKey: key, Ref: landedRef,
		BaseSHA: base, SnapshotSHA: landedSnapshot, PatchDigest: landedDigest,
		CreatedAt: time.Now().Add(-2 * time.Hour), RetainUntil: time.Now().Add(29 * 24 * time.Hour),
	}
	landedRetained, _, err := recovery.PublishToInventory(ctx, mirror, root, []string{manager.Root}, landedRecord, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	seedRecoveryLandingForRetirement(t, layout, mirror, landedRetained, "landed")

	// The inventory is now full (2/2). Drive one more cleanup — the
	// "MaxSnapshots + 1"th — for a brand-new run.
	option, err := recoveryCleanupOption(layout, cfg, workcopies, cloneURL, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	option(manager)

	const newRunID = "new-run"
	newRun, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: newRunID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newRun.Close() }()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: source, RunID: newRunID + "-stage", OwnerRunID: newRunID, BaseRef: "main", Branch: "goobers/implementation/" + newRunID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "new.txt"), []byte("new work"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatalf("cleanup did not finalize cleanly under a full inventory: %v (the fix must evict the terminal, landed entry to make room)", err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree was not removed after cleanup: %v", err)
	}

	// The landed entry must actually have been retired: its ref is gone from
	// the managed mirror (where WithRecoveryRepositoriesLocked operates).
	if gotRef := recoveryCLIGit(t, mirror, "for-each-ref", "--format=%(objectname)", landedRef); gotRef != "" {
		t.Fatalf("landed entry was not retired to make room: %s", gotRef)
	}
	// The still-running entry must be untouched.
	if gotRef := recoveryCLIGit(t, mirror, "for-each-ref", "--format=%(objectname)", runningRef); gotRef != runningSnapshot {
		t.Fatalf("in-flight entry was evicted instead of the landed one: got %q, want %q", gotRef, runningSnapshot)
	}
	// The new run's own entry must now be in the inventory, still capped at 2.
	entries, err := recovery.ReadInventory(ctx, root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("inventory has %d entries, want 2 (cap respected)", len(entries))
	}
	foundNew := false
	for _, entry := range entries {
		if entry.Record.RunID == newRunID {
			foundNew = true
		}
		if entry.Record.RunID == landedRunID {
			t.Fatal("retired entry is still an active inventory entry")
		}
	}
	if !foundNew {
		t.Fatal("new run's snapshot was not published despite a full-then-evicted inventory")
	}
}
