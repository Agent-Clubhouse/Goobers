package recovery

import (
	"context"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// GoneTargetEventKind marks the instance-journal record that a cleanup target
// was settled because its checkout directory no longer exists.
const GoneTargetEventKind = "recovery-cleanup-target-missing"

// SettleGoneCleanupTarget settles a cleanup whose checkout directory is
// already gone (#5383). A handoff that reads the checkout can never pass, so
// deferring on it would leak the cleanup entry forever. A linked worktree's
// abandoned preparation branch lives in the shared repository rather than the
// checkout, so it is still handed to the inventory from there, and the
// missing checkout is journaled as the reason, before cleanup proceeds.
// gone is false when the checkout exists and ordinary capture applies; an
// error leaves the cleanup deferred for retry.
func SettleGoneCleanupTarget(ctx context.Context, manager *worktree.Manager, target worktree.CleanupTarget, request RetentionRequest, log PublicationJournal) (gone bool, err error) {
	if _, err := os.Lstat(target.Path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect cleanup target: %w", err)
	}
	if log == nil {
		return true, fmt.Errorf("gone cleanup target requires a durable journal")
	}
	shared, err := goneTargetSharedRepository(manager, target)
	if err != nil {
		return true, err
	}
	source := "unavailable"
	if shared != "" {
		request.Repository = shared
		if err := RetainAbandonedPreparation(ctx, request, log); err != nil {
			return true, err
		}
		source = "shared"
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, RunID: target.OwnerRunID,
		Reason: "worktree cleanup target already removed; its checkout held nothing left to capture",
		Runner: map[string]any{"kind": GoneTargetEventKind, "worktreeId": target.WorktreeID, "preparationRepository": source}}
	if err := log.Append(event); err != nil {
		return true, fmt.Errorf("record missing cleanup target: %w", err)
	}
	return true, nil
}

// goneTargetSharedRepository returns "" when no repository can still hold the
// target's refs: a pinned clone's refs lived in the lost checkout itself.
func goneTargetSharedRepository(manager *worktree.Manager, target worktree.CleanupTarget) (string, error) {
	if manager == nil || target.Pinned {
		return "", nil
	}
	shared, ok := manager.LinkedWorktreeRepository(target.Path)
	if !ok {
		return "", nil
	}
	if _, err := os.Lstat(shared); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect shared cleanup repository: %w", err)
	}
	return shared, nil
}
