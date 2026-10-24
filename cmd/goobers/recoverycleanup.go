package main

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

func recoveryCleanupOption(layout instance.Layout, cfg *instance.Config, cleanupRoot string, cloneURL func(apiv1.RepoRef) (string, error), scrubber journal.Scrubber, tel *telemetry.Client) (worktree.ManagerOption, error) {
	return runtimewiring.RecoveryCleanupOption(layout, cfg, cleanupRoot, cloneURL, scrubber, tel, runtimewiring.RecoveryCleanupHooks{RetiredParent: retiredParentCleanup, ParentPolicy: parentCleanupPolicy})
}
func installTerminalRecoveryGuard(layout instance.Layout, manager *worktree.Manager) error {
	return runtimewiring.InstallTerminalRecoveryGuard(layout, manager, runtimewiring.RecoveryCleanupHooks{RetiredParent: retiredParentCleanup, ParentPolicy: parentCleanupPolicy})
}
