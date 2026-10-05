package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if got, ok := occupancies[branch]; !ok || got.OwnerRunID != owner ||
		got.Status != BranchOccupancyActive || !got.OwnerProcessLive {
		t.Fatalf("occupancy = %+v, want branch %q owned by live process for %q with active status", occupancies, branch, owner)
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

func TestBranchOccupanciesReportsDeadAndReusedOwnersNotLive(t *testing.T) {
	tests := []struct {
		name  string
		probe func(marker)
	}{
		{
			name: "dead",
			probe: func(marker) {
				processAlive = func(int) bool { return false }
			},
		},
		{
			name: "reused",
			probe: func(mk marker) {
				processAlive = func(int) bool { return true }
				processStartTime = func(int) (time.Time, bool) {
					return mk.PIDStartedAt.Add(pidReusedTolerance + time.Second), true
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			t.Cleanup(func() { _ = wt.Remove(ctx, RemoveOptions{}) })

			mk, err := readMarker(manager.markerPath(wt.key, wt.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if mk.PIDStartedAt.IsZero() {
				mk.PIDStartedAt = time.Now().UTC()
				ownershipPath := manager.ownershipPath(wt.key, filepath.Base(wt.Path))
				ownership, readErr := readMarker(ownershipPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				ownership.PIDStartedAt = mk.PIDStartedAt
				if err := writeMarker(ownershipPath, ownership); err != nil {
					t.Fatal(err)
				}
				if err := writeMarker(manager.markerPath(wt.key, wt.RunID), mk); err != nil {
					t.Fatal(err)
				}
			}

			previousAlive := processAlive
			previousStartTime := processStartTime
			tt.probe(mk)
			t.Cleanup(func() {
				processAlive = previousAlive
				processStartTime = previousStartTime
			})

			occupancies, err := manager.BranchOccupancies(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := occupancies[mk.Branch]; got.OwnerProcessLive {
				t.Fatalf("occupancy = %+v, want %s owner reported not live", got, tt.name)
			}
		})
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

// A registration whose directory vanished without `git worktree prune` (reaped
// or crashed run) is stale metadata, not an occupant. Production goobers-site
// merge-review failed on every run with "lstat ...: no such file or directory".
func TestBranchOccupanciesIgnoresPrunableRegistrationWithMissingDirectory(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	const branch = "goobers/implementation/gone-run"
	wt, err := manager.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "implementation-stage", OwnerRunID: "implementation-run",
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	occupancies, err := manager.BranchOccupancies(ctx, repo)
	if err != nil {
		t.Fatalf("BranchOccupancies with prunable registration: %v", err)
	}
	if _, ok := occupancies[branch]; ok {
		t.Fatalf("prunable registration reported as occupant: %+v", occupancies[branch])
	}
}
