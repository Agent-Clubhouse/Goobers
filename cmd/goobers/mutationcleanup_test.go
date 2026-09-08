package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestMutationCleanupGuardPersistsReceiptBeforeRemovingWorktree(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	repo, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "receipt-owner", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	manager, err := worktree.NewManager(layout.WorkcopiesDir(), mutationCleanupGuard(layout.RunsDir()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wt, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: repo, RunID: "receipt-owner-stage", OwnerRunID: "receipt-owner", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wt.Path, mutationsSidecarFile)
	data := []byte("{\"receiptId\":\"receipt-one\",\"provider\":\"github\",\"kind\":\"pr\",\"id\":\"9\",\"operation\":\"merge\"}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, worktree.RemoveOptions{}); !errors.Is(err, journal.ErrRecoveryBusy) {
		t.Fatalf("cleanup did not refuse busy journal: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("cleanup lost uncommitted receipt: %q %v", got, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed after durable handoff: %v", err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), "receipt-owner"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Runner["mutationReceiptId"] == "receipt-one" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("durable receipt count = %d, want 1", count)
	}
}

func TestPinnedCleanupImportsIntoPreviousOwnersJournal(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	repo, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "previous-owner", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	manager, err := worktree.NewManager(layout.WorkcopiesDir(), mutationCleanupGuard(layout.RunsDir()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	acquire := func(owner string) (*worktree.PinnedLease, error) {
		return manager.AcquirePinned(ctx, worktree.PinnedOptions{RepoURL: repo, RunID: owner, BaseRef: "main", CleanPolicy: worktree.PinnedCleanFull})
	}
	lease, err := acquire("previous-owner")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(lease.Worktree.Path, mutationsSidecarFile)
	data := []byte("{\"receiptId\":\"pinned-receipt\",\"provider\":\"github\",\"kind\":\"pr\",\"id\":\"9\",\"runId\":\"different-claim-owner\"}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := acquire("next-owner"); !errors.Is(err, journal.ErrRecoveryBusy) {
		if unexpected != nil {
			_ = unexpected.Release()
		}
		t.Fatalf("new owner bypassed pending old receipt: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("old receipt lost: %q %v", got, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err = acquire("next-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old receipt leaked to new run: %v", err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), "previous-owner"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 2 {
		t.Fatalf("old journal = %+v, %v", events, err)
	}
	if events[1].Runner["mutationReceiptId"] != "pinned-receipt" || events[1].Runner["claimRunId"] != "different-claim-owner" {
		t.Fatalf("ownership or receipt lost: %+v", events[1])
	}
}
