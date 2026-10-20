package worktree

import (
	"context"
	"sync"
	"time"
)

// Read-only custody checks run during HTTP admission. They must not outlive
// the request budget behind another operation on the same repository.
func lockWithContext(ctx context.Context, held *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if held.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if held.TryLock() {
				return nil
			}
		}
	}
}
