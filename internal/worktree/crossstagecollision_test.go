package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerCreateReconcilesReleasedSameRunBranchOccupant(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/implementation/workflow-run"
	)

	guardCalls := 0
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		guardCalls++
		if guardCalls == 1 {
			return errors.New("temporary recovery outage")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("create first stage: %v", err)
	}
	mustWriteFile(t, filepath.Join(first.Path, "stage-a.txt"), "branch continuity\n")
	runTestGit(t, first.Path, "add", "stage-a.txt")
	runTestGit(t, first.Path, "commit", "-m", "stage A")
	if err := first.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("first Remove error = %v, want deferred cleanup", err)
	}
	assertCleanupPendingRecords(t, m, first.key, first.RunID)

	second, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("create next stage after deferred teardown: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(second.Path, "stage-a.txt")); err != nil || string(got) != "branch continuity\n" {
		t.Fatalf("next stage lost prior branch commit: %q, %v", got, err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("released first-stage path survived reconciliation: %v", err)
	}
	if _, err := os.Stat(m.markerPath(first.key, first.RunID)); !os.IsNotExist(err) {
		t.Fatalf("released first-stage marker survived reconciliation: %v", err)
	}
	if guardCalls != 2 {
		t.Fatalf("cleanup guard calls = %d, want failed teardown plus successful retry", guardCalls)
	}
	if err := second.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove second stage: %v", err)
	}
}

func TestManagerCreateRefusesLiveSameRunBranchOccupant(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/implementation/workflow-run"
	)
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(first.Path, "live.txt"), "do not remove\n")

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), "has not surrendered") {
		t.Fatalf("Create with live same-run occupant error = %v, want refusal", err)
	}
	if got, err := os.ReadFile(filepath.Join(first.Path, "live.txt")); err != nil || string(got) != "do not remove\n" {
		t.Fatalf("live occupant changed: %q, %v", got, err)
	}
}

func TestManagerCreateRefusesForeignReleasedBranchOccupant(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const branch = "goobers/implementation/shared-name"
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("recovery unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "owner-a-stage", OwnerRunID: "owner-a", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(first.Path, "foreign.txt"), "preserve\n")
	if err := first.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("first Remove error = %v, want deferred cleanup", err)
	}

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "owner-b-stage", OwnerRunID: "owner-b", BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), "owned by another run") {
		t.Fatalf("Create with foreign occupant error = %v, want ownership refusal", err)
	}
	if got, err := os.ReadFile(filepath.Join(first.Path, "foreign.txt")); err != nil || string(got) != "preserve\n" {
		t.Fatalf("foreign occupant changed: %q, %v", got, err)
	}
}

func TestManagerCreateRefusesKeptSameRunBranchOccupant(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/review/workflow-run"
	)
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(first.Path, "kept.txt"), "preserve\n")
	if err := first.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep first workspace: %v", err)
	}

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), `status "kept"`) {
		t.Fatalf("Create with kept occupant error = %v, want kept refusal", err)
	}
	if got, err := os.ReadFile(filepath.Join(first.Path, "kept.txt")); err != nil || string(got) != "preserve\n" {
		t.Fatalf("kept occupant changed: %q, %v", got, err)
	}
}

func TestManagerCreatePreservesReleasedOccupantWhenCleanupRetryFails(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/review/workflow-run"
	)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("still unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(first.Path, "pending.txt"), "preserve\n")
	if err := first.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("first Remove error = %v, want deferred cleanup", err)
	}

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("Create retry error = %v, want deferred cleanup", err)
	}
	if got, err := os.ReadFile(filepath.Join(first.Path, "pending.txt")); err != nil || string(got) != "preserve\n" {
		t.Fatalf("deferred occupant changed: %q, %v", got, err)
	}
	assertCleanupPendingRecords(t, m, first.key, first.RunID)
}

func TestManagerCreateRefusesDisagreeingReleasedOwnershipRecords(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/review/workflow-run"
	)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("defer cleanup")
	}); err != nil {
		t.Fatal(err)
	}
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("first Remove error = %v, want deferred cleanup", err)
	}
	primary, err := readMarker(m.markerPath(first.key, first.RunID))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := primary.directoryName()
	if err != nil {
		t.Fatal(err)
	}
	ownershipPath := m.ownershipPath(first.key, directory)
	ownership, err := readMarker(ownershipPath)
	if err != nil {
		t.Fatal(err)
	}
	ownership.Branch = "goobers/review/some-other-branch"
	if err := writeMarker(ownershipPath, ownership); err != nil {
		t.Fatal(err)
	}

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), "ownership records disagree") {
		t.Fatalf("Create with disagreeing records error = %v, want refusal", err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("disputed occupant was removed: %v", err)
	}
}

func TestManagerCreateRefusesTraversingReleasedOwnershipRunID(t *testing.T) {
	for _, maliciousRunID := range []string{"../escaped", `..\escaped`} {
		t.Run(strings.ReplaceAll(maliciousRunID, `\`, "backslash"), func(t *testing.T) {
			ctx := context.Background()
			repo := newSourceRepo(t)
			m := newTestManager(t)
			const (
				owner  = "workflow-run"
				branch = "goobers/review/workflow-run"
			)
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
				return errors.New("defer cleanup")
			}); err != nil {
				t.Fatal(err)
			}
			first, err := m.Create(ctx, CreateOptions{
				RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
				t.Fatalf("first Remove error = %v, want deferred cleanup", err)
			}

			// Recreate the hostile disk shape precisely: the branch occupant is
			// still contained under runs/, but its ownership record hashes a RunID
			// that would escape markers/ on one supported platform.
			directory := worktreeDirectoryName(maliciousRunID)
			hostilePath := filepath.Join(m.runsDirForKey(first.key), directory)
			runTestGit(t, m.repoDirForKey(first.key), "worktree", "move", first.Path, hostilePath)
			ownership, err := readMarker(m.ownershipPath(first.key, filepath.Base(first.Path)))
			if err != nil {
				t.Fatal(err)
			}
			ownership.RunID = maliciousRunID
			ownership.Directory = directory
			if err := writeMarker(m.ownershipPath(first.key, directory), ownership); err != nil {
				t.Fatal(err)
			}
			escapedPrimary := m.markerPath(first.key, maliciousRunID)
			if err := writeMarker(escapedPrimary, ownership); err != nil {
				t.Fatal(err)
			}

			_, err = m.Create(ctx, CreateOptions{
				RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
			})
			if err == nil || !strings.Contains(err.Error(), "ownership directory identity is invalid") {
				t.Fatalf("Create with traversing ownership RunID error = %v, want identity refusal", err)
			}
			if _, err := os.Stat(hostilePath); err != nil {
				t.Fatalf("traversing ownership removed occupant: %v", err)
			}
			if _, err := os.Stat(escapedPrimary); err != nil {
				t.Fatalf("traversing ownership removed primary evidence: %v", err)
			}
		})
	}
}

func TestManagerCreateRefusesBranchOccupantOutsideManagedRuns(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const (
		owner  = "workflow-run"
		branch = "goobers/review/workflow-run"
	)
	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-a", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	externalParent := t.TempDir()
	externalPath := filepath.Join(externalParent, filepath.Base(first.Path))
	runTestGit(t, m.repoDirForKey(first.key), "worktree", "move", first.Path, externalPath)

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: owner + "-stage-b", OwnerRunID: owner, BaseRef: "main", Branch: branch,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the managed runs directory") {
		t.Fatalf("Create with external occupant error = %v, want containment refusal", err)
	}
	if _, err := os.Stat(externalPath); err != nil {
		t.Fatalf("external occupant was removed: %v", err)
	}
}

func TestManagerReapRetriesCleanupPendingWithLiveDaemonPID(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	blocked := true
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		if blocked {
			return errors.New("temporary outage")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "workflow-run-stage", OwnerRunID: "workflow-run", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("Remove error = %v, want deferred cleanup", err)
	}
	assertCleanupPendingRecords(t, m, wt.key, wt.RunID)
	blocked = false

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Reap = %+v warnings=%+v err=%v", results, warnings, err)
	}
	if len(results) != 1 || results[0].RunID != wt.RunID || results[0].Reason != ReapReasonOrphaned {
		t.Fatalf("Reap results = %+v, want cleanup-pending worktree", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("cleanup-pending worktree survived Reap: %v", err)
	}
}

func TestManagerFinalizeRunRetriesCleanupPending(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	blocked := true
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		if blocked {
			return errors.New("temporary outage")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "workflow-run-stage", OwnerRunID: "workflow-run", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("Remove error = %v, want deferred cleanup", err)
	}
	blocked = false

	results, err := m.FinalizeRun(ctx, "workflow-run")
	if err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}
	if len(results) != 1 || results[0].WorktreeID != wt.RunID || results[0].Kept {
		t.Fatalf("FinalizeRun results = %+v, want cleanup-pending worktree removed", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("cleanup-pending worktree survived FinalizeRun: %v", err)
	}
}

func assertCleanupPendingRecords(t *testing.T, m *Manager, key, runID string) {
	t.Helper()
	primary, err := readMarker(m.markerPath(key, runID))
	if err != nil {
		t.Fatalf("read primary marker: %v", err)
	}
	directory, err := primary.directoryName()
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := readMarker(m.ownershipPath(key, directory))
	if err != nil {
		t.Fatalf("read ownership marker: %v", err)
	}
	if primary.Status != statusCleanupPending || ownership.Status != statusCleanupPending ||
		!sameWorkspaceIdentity(primary, ownership) {
		t.Fatalf("cleanup-pending records disagree: primary=%+v ownership=%+v", primary, ownership)
	}
}
