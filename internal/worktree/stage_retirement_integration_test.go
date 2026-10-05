//go:build integration

package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationStageRetirementPreservesTamperedAndReplaysCleanup(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"held", "released", "pending", "ownership-leftover", "changed-owner"} {
		t.Run(mode, func(t *testing.T) {
			source := newSourceRepo(t)
			manager := newTestManager(t)
			wt, err := manager.Create(t.Context(), CreateOptions{RepoURL: source, RunID: "retiring-stage", OwnerRunID: "parent", BaseRef: "main", Branch: "runs/parent"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("contribution"), 0600); err != nil {
				t.Fatal(err)
			}
			custody, err := wt.HoldForChild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "changed-owner" {
				changed := custody
				changed.OwnerRunID = "another"
				if err := manager.RetireHeldStage(t.Context(), source, changed); err == nil {
					t.Fatal("foreign custody retired")
				}
				if _, err := os.Stat(filepath.Join(wt.Path, "new.txt")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "released" || mode == "pending" || mode == "ownership-leftover" {
				if err := wt.ReleaseChildHold(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "pending" || mode == "ownership-leftover" {
				primary, ownership, err := wt.custodyMarkers()
				if err != nil {
					t.Fatal(err)
				}
				primary.Status, ownership.Status = statusCleanupPending, statusCleanupPending
				if err := wt.writeCustodyMarkers(primary, ownership); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ownership-leftover" {
				runTestGit(t, manager.repoDirForKey(wt.key), "worktree", "remove", "--force", wt.Path)
				if err := os.Remove(manager.markerPath(wt.key, wt.RunID)); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := manager.RetireHeldStage(t.Context(), source, custody); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
				t.Fatal("retirement left checkout", err)
			}
			if _, err := os.Stat(manager.ownershipPath(wt.key, filepath.Base(wt.Path))); !os.IsNotExist(err) {
				t.Fatal("retirement left ownership", err)
			}
		})
	}
}
