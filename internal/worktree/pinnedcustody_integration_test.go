//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPinnedCustodyPrecedesDestructivePreparation(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"remove", "stage", "release", "replace", "reset"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			manager, repo := pinnedFixture(t)
			lease := acquirePinnedFixture(t, manager, repo, "previous-run", PinnedCleanNone)
			defer func() { _ = lease.Release() }()
			path := lease.Worktree.Path
			mustWriteFile(t, filepath.Join(path, "README.md"), "uncommitted implementation")
			before := strings.TrimSpace(runTestGit(t, path, "rev-parse", "HEAD"))
			ownerPath := filepath.Join(manager.pinnedRoot, repoKey(repo), pinnedCustodyFile)
			original, err := os.ReadFile(ownerPath)
			if err != nil {
				t.Fatal(err)
			}
			blocked := errors.New("recovery archive unavailable")
			var target CleanupTarget
			if mode == "replace" || mode == "reset" {
				if err := lease.Release(); err != nil {
					t.Fatal(err)
				}
				lease = nil
			}
			if err := manager.SetCleanupGuard("recovery", func(_ context.Context, got CleanupTarget) error {
				target = got
				return blocked
			}); err != nil {
				t.Fatal(err)
			}
			var failure error
			switch mode {
			case "release":
				failure = lease.Release()
			case "remove":
				failure = lease.Worktree.Remove(ctx, RemoveOptions{})
			case "stage":
				failure = lease.Worktree.PreparePinned(ctx, PinnedPrepareOptions{BaseRef: "main", Branch: lease.Worktree.Branch})
			case "replace":
				_, failure = manager.AcquirePinned(ctx, PinnedOptions{RepoURL: repo, RunID: "next-run", BaseRef: "main", Branch: "goobers/test/next-run"})
			case "reset":
				_, failure = manager.ResetPinned(ctx, PinnedResetOptions{RepoURL: repo, BaseRef: "main"})
			}
			if !errors.Is(failure, blocked) || !errors.Is(failure, ErrCleanupDeferred) {
				t.Fatalf("cleanup did not defer: %v", failure)
			}
			if !target.Pinned || target.OwnerRunID != "previous-run" || target.RepositoryDigest != RepositoryDigest(repo) || target.Path != path {
				t.Fatalf("lost previous custody: %+v", target)
			}
			if got := strings.TrimSpace(runTestGit(t, path, "rev-parse", "HEAD")); got != before {
				t.Fatal("HEAD changed before acknowledgement")
			}
			if data, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(data) != "uncommitted implementation" {
				t.Fatalf("implementation lost: %q %v", data, err)
			}
			if data, err := os.ReadFile(ownerPath); err != nil || string(data) != string(original) {
				t.Fatalf("custody changed before acknowledgement: %q %v", data, err)
			}
			if mode == "release" {
				_, err := manager.AcquirePinned(ctx, PinnedOptions{RepoURL: repo, RunID: "next-run", BaseRef: "main"})
				var stale *StalePinnedLeaseError
				if !errors.As(err, &stale) || stale.RunID != "previous-run" {
					t.Fatalf("failed handoff allowed new lease: %v", err)
				}
			}
		})
	}
}

func TestIntegrationPinnedCustodyTransfersOnlyAfterAcknowledgement(t *testing.T) {
	testdep.Require(t, "git")
	manager, repo := pinnedFixture(t)
	lease := acquirePinnedFixture(t, manager, repo, "previous-run", PinnedCleanNone)
	path := lease.Worktree.Path
	mustWriteFile(t, filepath.Join(path, "README.md"), "implementation to retain")
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	var acknowledged bool
	if err := manager.SetCleanupGuard("recovery", func(_ context.Context, target CleanupTarget) error {
		if target.OwnerRunID != "previous-run" {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(target.Path, "README.md"))
		if err != nil || string(data) != "implementation to retain" {
			t.Fatalf("handoff saw reset state: %q %v", data, err)
		}
		acknowledged = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next := acquirePinnedFixture(t, manager, repo, "next-run", PinnedCleanNone)
	defer func() { _ = next.Release() }()
	if !acknowledged {
		t.Fatal("replacement skipped handoff")
	}
	owner, err := readMarker(filepath.Join(manager.pinnedRoot, repoKey(repo), pinnedCustodyFile))
	if err != nil || owner.OwnerRunID != "next-run" || owner.RepositoryDigest != RepositoryDigest(repo) {
		t.Fatalf("new custody not published: %+v %v", owner, err)
	}
}

func TestIntegrationPinnedReleaseCannotReenterAfterLeaseTransfer(t *testing.T) {
	testdep.Require(t, "git")
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "same-run", PinnedCleanNone)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	next := acquirePinnedFixture(t, manager, repo, "same-run", PinnedCleanNone)
	defer func() { _ = next.Release() }()
	calls := 0
	if err := manager.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("released lease recaptured a new owner's active workspace")
	}
	recordPath := filepath.Join(manager.pinnedRoot, repoKey(repo), "pin.lease.json")
	data, err := os.ReadFile(recordPath)
	if err != nil || !strings.Contains(string(data), "same-run") {
		t.Fatalf("old release cleared new lease: %q %v", data, err)
	}
}
