package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// classifyRecoveryCapacityGuard wraps a recovery cleanup guard so that a
// refusal caused by a full recovery inventory carries
// worktree.ErrCleanupRecoveryCapacity in its chain (#5264).
//
// The dependency is inverted here rather than inside internal/worktree because
// worktree cannot import internal/recovery — recovery reaches worktree
// transitively (recovery -> apicontract -> readservice -> creditgraph ->
// telemetry -> worktree), so that direction is an import cycle. cmd/goobers
// already imports both and is where these guards are registered, which makes it
// the one place that can translate between the two sentinels without either
// package matching the other's message prose.
//
// Why the distinction is worth carrying: a capacity refusal and a handoff still
// in progress both surface as a deferred cleanup, but they need opposite
// operator responses — free recovery capacity versus simply wait. Reported as
// one undifferentiated warning, the only way to tell them apart was to read the
// wrapped error text.
func classifyRecoveryCapacityGuard(
	callback func(context.Context, worktree.CleanupTarget) error,
) func(context.Context, worktree.CleanupTarget) error {
	if callback == nil {
		return nil
	}
	return func(ctx context.Context, target worktree.CleanupTarget) error {
		err := callback(ctx, target)
		if err == nil || !errors.Is(err, recovery.ErrInventoryFull) {
			return err
		}
		// Wrapped, never replaced: the original error carries the slot counts
		// and the inventory path an operator needs, and recoveryInventoryReadError
		// may already have attached the remediation guidance.
		return fmt.Errorf("%w: %w", worktree.ErrCleanupRecoveryCapacity, err)
	}
}
