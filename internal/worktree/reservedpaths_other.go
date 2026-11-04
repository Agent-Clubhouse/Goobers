//go:build !windows

package worktree

import "context"

func removeWindowsReservedCleanupPaths(context.Context, string) error {
	return nil
}
