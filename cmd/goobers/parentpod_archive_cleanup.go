package main

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

// Runs inside the managed repository cleanup lock. Never reacquire that lock
// through WithRecoveryMirror; the manager-derived shared path is already owned.
func retiredParentCleanup(ctx context.Context, layout instance.Layout, cfg *instance.Config, manager *worktree.Manager, reader *journal.Reader, key string, target worktree.CleanupTarget) (bool, error) {
	archive, found, err := runner.ParentCleanupArchive(reader, target)
	if err != nil || !found {
		return found, err
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return true, err
	}
	if phase == journal.PhaseRunning {
		return true, errors.New("retired parent cleanup requires a terminal owner")
	}
	restorer := parentArchiveRestorer{layout: layout, config: cfg}
	record, contract, err := restorer.authorize(ctx, reader, archive)
	if err != nil {
		return true, err
	}
	if record.RepositoryKey != key || contract.Workspace == nil {
		return true, errors.New("retired parent cleanup source changed")
	}
	if err := verifyParentArchiveChildren(ctx, layout, contract.Identity); err != nil {
		return true, err
	}
	path, err := parentArchiveInventoryPath(ctx, layout, record)
	if err != nil {
		return true, err
	}
	repository, ok := manager.LinkedWorktreeRepository(target.Path)
	if !ok {
		return true, errors.New("retired parent cleanup requires its managed mirror")
	}
	policy, _ := resolveRecoveryPolicy(layout, cfg)
	maxBytes := policy.MaxArchiveBytesEffective()
	if err := recovery.ImportSnapshotBundle(ctx, repository, path, record, maxBytes); err != nil {
		return true, err
	}
	state, err := recovery.ReadRetainedParentState(ctx, repository, record)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(state.Policy, contract.Workspace.Snapshot.Policy) {
		return true, errors.New("retired parent cleanup policy changed")
	}
	info, err := os.Lstat(target.Path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if !info.IsDir() {
		return true, errors.New("retired parent cleanup target is not a directory")
	}
	return true, recovery.VerifyRetainedParentCheckout(ctx, target.Path, record, maxBytes)
}

func verifyParentArchiveChildren(ctx context.Context, layout instance.Layout, id journal.RunIdentity) error {
	service, ok := stageGrantMinterFor(layout.Root).(*daemonCredentialService)
	if !ok || service.childQueue == nil {
		return errors.New("parent archive child custody service unavailable")
	}
	parent := triggerqueue.ChildParent{Gaggle: id.Gaggle, ParentRunID: id.RunID}
	for after := ""; ; {
		children, err := service.childQueue.Children(ctx, parent, after, 100)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.State.Terminal() || child.AcknowledgedAt.IsZero() {
				return errors.New("parent archive has unfinished or unacknowledged child work")
			}
			after = child.ChildID
		}
		if len(children) < 100 {
			return nil
		}
	}
}
