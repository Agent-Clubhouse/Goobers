package worktree

import (
	"context"
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
