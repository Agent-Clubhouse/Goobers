package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
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
	record, authority, err := restorer.authorize(ctx, reader, archive)
	if err != nil {
		return true, err
	}
	if record.RepositoryKey != key {
		return true, errors.New("retired parent cleanup source changed")
	}
	if err := verifyParentArchiveChildren(ctx, layout, authority.identity); err != nil {
		return true, err
	}
	repository, ok := manager.LinkedWorktreeRepository(target.Path)
	if !ok {
		return true, errors.New("retired parent cleanup requires its managed mirror")
	}
	policy, _ := resolveRecoveryPolicy(layout, cfg)
	maxBytes := policy.MaxArchiveBytesEffective()
	state, err := recovery.LoadRetainedParentState(ctx, repository, filepath.Join(layout.Root, "recovery"), recoveryOverflowRoot(layout), record, maxBytes)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(state.Policy, authority.policy) {
		return true, errors.New("retired parent cleanup policy changed")
	}
	if gone, err := recovery.CleanupTargetGone(target.Path); gone || err != nil {
		return true, err
	}
	info, err := os.Lstat(target.Path)
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
				return fmt.Errorf("%w: parent archive has unfinished or unacknowledged child work", invoke.ErrChildCustodyPending)
			}
			after = child.ChildID
		}
		if len(children) < 100 {
			return nil
		}
	}
}
