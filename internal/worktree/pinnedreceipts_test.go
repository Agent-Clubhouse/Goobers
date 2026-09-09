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
			err := manager.SetCleanupGuard(MutationReceiptGuard, func(_ context.Context, target CleanupTarget) error {
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
			})
			if err != nil {
				t.Fatal(err)
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

func TestPinnedReceiptsRequireReceiptSpecificAcknowledgment(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "previous-owner", PinnedCleanNone)
	path := filepath.Join(first.Worktree.Path, "mutations.jsonl")
	if err := os.WriteFile(path, []byte("unacknowledged receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquirePinned(context.Background(), PinnedOptions{RepoURL: repo, RunID: "next-owner", BaseRef: "main", CleanPolicy: PinnedCleanFull})
	if err == nil {
		_ = lease.Release()
		t.Fatal("unrelated recovery ACK authorized deleting mutation receipts")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "unacknowledged receipt" {
		t.Fatalf("unacknowledged receipt lost: %q %v", data, err)
	}
}

func TestReceiptAcknowledgmentUsesExecutedGuardSnapshot(t *testing.T) {
	manager := &Manager{}
	receiptCalls := 0
	if err := manager.SetCleanupGuard("other", func(context.Context, CleanupTarget) error {
		return manager.SetCleanupGuard(MutationReceiptGuard, func(context.Context, CleanupTarget) error { receiptCalls++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	target := CleanupTarget{Path: "unused", WorktreeID: "worktree", OwnerRunID: "run"}
	acknowledged, err := manager.prepareCleanupTargetWithReceipts(context.Background(), target)
	if err != nil || acknowledged || receiptCalls != 0 {
		t.Fatalf("newly registered guard was treated as executed: ack=%t calls=%d err=%v", acknowledged, receiptCalls, err)
	}
	acknowledged, err = manager.prepareCleanupTargetWithReceipts(context.Background(), target)
	if err != nil || !acknowledged || receiptCalls != 1 {
		t.Fatalf("next cleanup did not execute receipt guard: ack=%t calls=%d err=%v", acknowledged, receiptCalls, err)
	}
}

func TestPinnedResetPreservesConsistentCustodyForNextRun(t *testing.T) {
	manager, repo := pinnedFixture(t)
	if err := manager.SetCleanupGuard(MutationReceiptGuard, func(context.Context, CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}
	first := acquirePinnedFixture(t, manager, repo, "previous-owner", PinnedCleanNone)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResetPinned(t.Context(), PinnedResetOptions{RepoURL: repo, BaseRef: "main"}); err != nil {
		t.Fatal(err)
	}
	next, err := manager.AcquirePinned(t.Context(), PinnedOptions{RepoURL: repo, RunID: "next-owner", BaseRef: "main", CleanPolicy: PinnedCleanFull})
	if err != nil {
		t.Fatalf("operator reset split receipt and recovery ownership: %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}
