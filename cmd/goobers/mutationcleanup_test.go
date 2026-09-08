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
