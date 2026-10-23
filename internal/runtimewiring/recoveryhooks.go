package runtimewiring

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/workspaceauthority"
	"github.com/goobers/goobers/internal/worktree"
)

// RecoveryCleanupHooks supplies daemon parent-workspace authority. A cloud
// worker has no local parent service and refuses parent cleanup without it.
type RecoveryCleanupHooks struct {
	RetiredParent func(context.Context, instance.Layout, *instance.Config, *worktree.Manager, *journal.Reader, string, worktree.CleanupTarget) (bool, error)
	ParentPolicy  func(context.Context, *journal.Reader, worktree.CleanupTarget, string) (*recovery.SnapshotPolicy, error)
}

func cleanupHooks(options []RecoveryCleanupHooks) RecoveryCleanupHooks {
	if len(options) > 0 {
		return options[0]
	}
	return RecoveryCleanupHooks{}
}
func (h RecoveryCleanupHooks) retiredParent(ctx context.Context, layout instance.Layout, cfg *instance.Config, manager *worktree.Manager, reader *journal.Reader, key string, target worktree.CleanupTarget) (bool, error) {
	if h.RetiredParent != nil {
		return h.RetiredParent(ctx, layout, cfg, manager, reader, key, target)
	}
	_, found, err := workspaceauthority.ParentCleanupArchive(reader, target)
	if err != nil || !found {
		return found, err
	}
	return true, fmt.Errorf("retired parent cleanup requires daemon parent authority")
}
func (h RecoveryCleanupHooks) parentPolicy(ctx context.Context, reader *journal.Reader, target worktree.CleanupTarget, key string) (*recovery.SnapshotPolicy, error) {
	if h.ParentPolicy != nil {
		return h.ParentPolicy(ctx, reader, target, key)
	}
	_, found, err := workspaceauthority.ParentCleanupWorkspace(reader, target)
	if err != nil || !found {
		return nil, err
	}
	return nil, fmt.Errorf("parent cleanup requires daemon parent authority")
}
