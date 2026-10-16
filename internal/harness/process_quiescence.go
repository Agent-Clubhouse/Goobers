package harness

import (
	"context"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// Strict custody invocations share process-owner evidence with deterministic
// executors. The callback follows process wait, never context cancellation.
func workspaceWriterStop(ctx context.Context, tree *proc.Tree) (stop func() error, joined func()) {
	return invoke.TrackWorkspaceProcess(ctx, tree, groupKillWaitDelay)
}
