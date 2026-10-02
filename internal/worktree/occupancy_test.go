package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchOccupanciesReportsAndReleasesActiveOwner(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	const (
		branch = "goobers/implementation/live-run"
		owner  = "implementation-run"
	)
	wt, err := manager.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: owner,
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}

	occupancies, err := manager.BranchOccupancies(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := occupancies[branch]; !ok || got.OwnerRunID != owner || got.Status != BranchOccupancyActive {
		t.Fatalf("occupancy = %+v, want branch %q owned by %q with active status", occupancies, branch, owner)
	}

	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	occupancies, err = manager.BranchOccupancies(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := occupancies[branch]; ok {
		t.Fatalf("released branch remains occupied: %+v", occupancies[branch])
	}
}

func TestBranchOccupanciesIgnoresEntryReleasedAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	wt, err := manager.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: "implementation-run",
		BaseRef: "main", Branch: "goobers/implementation/live-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := registeredWorktrees(ctx, manager.repoDirForKey(wt.key))
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	occupancies, err := manager.branchOccupanciesFromEntries(
		ctx, repo, wt.key, manager.repoDirForKey(wt.key), entries,
	)
	if err != nil {
		t.Fatalf("inspect stale snapshot after release: %v", err)
	}
	if len(occupancies) != 0 {
		t.Fatalf("occupancies = %+v, want released snapshot entry ignored", occupancies)
	}
}

func TestBranchOccupanciesRejectsStableMissingOwnershipRecord(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	wt, err := manager.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: "implementation-run",
		BaseRef: "main", Branch: "goobers/implementation/live-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	ownershipPath := manager.ownershipPath(wt.key, filepath.Base(wt.Path))
	ownership, err := readMarker(ownershipPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ownershipPath); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writeMarker(ownershipPath, ownership); err != nil {
			t.Errorf("restore ownership record: %v", err)
			return
		}
		if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
			t.Errorf("remove worktree: %v", err)
		}
	}()

	_, err = manager.BranchOccupancies(ctx, repo)
	if err == nil || !strings.Contains(err.Error(), "ownership record") {
		t.Fatalf("BranchOccupancies error = %v, want stable missing ownership record", err)
	}
}
