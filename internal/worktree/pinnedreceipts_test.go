package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedReceiptsKeepPreviousOwnerAcrossLeaseRelease(t *testing.T) {
	for _, mode := range []string{"next-run", "explicit-reset"} {
		t.Run(mode, func(t *testing.T) {
			manager, repo := pinnedFixture(t)
			first := acquirePinnedFixture(t, manager, repo, "previous-owner", PinnedCleanNone)
			path := filepath.Join(first.Worktree.Path, "mutations.jsonl")
			if err := os.WriteFile(path, []byte("receipt"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := first.Release(); err != nil {
				t.Fatal(err)
			}
			blocked := errors.New("receipt journal unavailable")
			deny := true
			manager.beforeCleanup = func(_ context.Context, target CleanupTarget) error {
				if target.OwnerRunID != "previous-owner" {
					t.Fatalf("lost prior run owner: %+v", target)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("receipt removed before handoff: %v", err)
				}
				if deny {
					return blocked
				}
				return nil
			}
			prepare := func() error {
				if mode == "explicit-reset" {
					_, err := manager.ResetPinned(context.Background(), PinnedResetOptions{RepoURL: repo, BaseRef: "main"})
					return err
				}
				lease, err := manager.AcquirePinned(context.Background(), PinnedOptions{RepoURL: repo, RunID: "next-owner", BaseRef: "main", CleanPolicy: PinnedCleanFull})
				if err == nil {
					err = lease.Release()
				}
				return err
			}
			if err := prepare(); !errors.Is(err, blocked) {
				t.Fatalf("preparation bypassed handoff: %v", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "receipt" {
				t.Fatalf("receipt lost: %q %v", data, err)
			}
			deny = false
			if err := prepare(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("old receipt carried into next owner: %v", err)
			}
		})
	}
}
