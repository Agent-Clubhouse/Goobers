//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCleanupHandoffPrecedesRollbackAndDeletion(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"remove", "keep", "reap", "replace", "finalize"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			blocked := errors.New("archive unavailable")
			deny := true
			var targets []CleanupTarget
			manager, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.SetCleanupGuard("recovery", func(_ context.Context, target CleanupTarget) error {
				targets = append(targets, target)
				if deny {
					return blocked
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			wt, err := manager.Create(ctx, CreateOptions{RepoURL: newSourceRepo(t), RunID: "owner-stage", OwnerRunID: "owner", Gaggle: "issue:42", BaseRef: "main", Branch: "goobers/impl/owner"})
			if err != nil {
				t.Fatal(err)
			}
			if err := wt.ActivateAssetPathGuard(); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(wt.Path, gooberassets.WorkspaceDir, "reference.md"), "private bundle")
			runTestGit(t, wt.Path, "add", "-f", gooberassets.WorkspaceDir)
			runTestGit(t, wt.Path, "commit", "-m", "guarded state")
			before := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
			evidence := filepath.Join(wt.Path, "dirty.txt")
			mustWriteFile(t, evidence, "uncommitted work")
			markerPath := manager.markerPath(wt.key, wt.RunID)
			acquisition := filepath.Join(manager.branchAcquisitionRunDir(wt.key, "owner"), "evidence.json")
			mustWriteFile(t, acquisition, "branch acquisition evidence")
			mk, err := readMarker(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			cleanup := func() error {
				switch mode {
				case "remove", "keep":
					return wt.Remove(ctx, RemoveOptions{Keep: mode == "keep"})
				case "reap":
					return manager.reapOne(ctx, wt.key, wt.Path, markerPath, &mk)
				case "finalize":
					_, err := manager.FinalizeRun(ctx, "owner")
					return err
				default:
					return manager.forceClear(ctx, wt.key, wt.Path, wt.RunID)
				}
			}
			if err := cleanup(); !errors.Is(err, blocked) {
				t.Fatalf("cleanup bypassed failed handoff: %v", err)
			}
			if got := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD")); got != before {
				t.Fatalf("rollback preceded handoff: got %s, want %s", got, before)
			}
			if data, err := os.ReadFile(evidence); err != nil || string(data) != "uncommitted work" {
				t.Fatalf("dirty evidence lost: %q %v", data, err)
			}
			if got, err := readMarker(markerPath); err != nil || got.OwnerRunID != "owner" {
				t.Fatalf("ownership evidence lost: %+v %v", got, err)
			}
			if data, err := os.ReadFile(acquisition); err != nil || string(data) != "branch acquisition evidence" {
				t.Fatalf("branch acquisition evidence lost: %q %v", data, err)
			}
			if len(targets) != 1 || targets[0] != (CleanupTarget{Path: wt.Path, WorktreeID: wt.RunID, OwnerRunID: "owner", Gaggle: "issue:42", BaseRef: "refs/heads/main", RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt}) || mk.RepositoryDigest == "" || mk.CreatedAt.IsZero() {
				t.Fatalf("incorrect handoff identity: %+v", targets)
			}
			deny = false
			if err := cleanup(); err != nil {
				t.Fatalf("acknowledged cleanup failed: %v", err)
			}
		})
	}
}
