package childengine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/localscheduler"
)

// ParentSlots is the control role's scheduler as the capacity plane needs it:
// lending a parked parent's concurrency slot back to the pool. The scheduler
// that admitted the run holds that slot for the run's whole life
// (internal/cloud/schedstart), exactly as the local daemon's does.
type ParentSlots interface {
	SuspendChildParent(ctx context.Context, runID string) (*localscheduler.ChildParentSuspension, error)
}

// SchedulerCapacity is the ParentCapacity over the control role's scheduler
// (docs/design/engine-child-workflows.md §6). Release lends the parked
// parent's slot back; Reacquire takes a slot again before the parent's
// writers restart. Capacity refused on reacquire leaves the parent parked and
// is unavailable, so the engine retries with backoff; it never mints a slot.
//
// Both are idempotent across retries and lost acknowledgements. The
// suspension of each parked parent is kept, so a retried Reacquire, after the
// slot was retaken, resumes the same suspension (a no-op) instead of
// suspending the running parent again. A successor epoch rebuilds a parked
// parent as suspended from its journal (schedstart.OpenRun.Suspended), so a
// Reacquire it has no suspension for suspends without releasing anything,
// then resumes.
type SchedulerCapacity struct {
	Slots ParentSlots

	mu        sync.Mutex
	suspended map[string]*localscheduler.ChildParentSuspension
}

var _ ParentCapacity = (*SchedulerCapacity)(nil)

// Release lends the parked parent's concurrency slot back to the pool.
func (c *SchedulerCapacity) Release(ctx context.Context, in engine.ChildParkInput) error {
	_, err := c.suspension(ctx, in.Binding.Request.ParentRunID, true)
	return err
}

// Reacquire takes a concurrency slot for the parent again.
func (c *SchedulerCapacity) Reacquire(ctx context.Context, in engine.ChildCompletionInput) error {
	runID := in.Binding.Request.ParentRunID
	suspension, err := c.suspension(ctx, runID, false)
	if err != nil {
		return err
	}
	if err := suspension.Resume(ctx); err != nil {
		var refused *localscheduler.TriggerRejectedError
		if errors.As(err, &refused) {
			return fmt.Errorf("%w: parent %s waits for capacity: %w", engine.ErrChildCustodyUnavailable, runID, err)
		}
		// The parent's admission ended (cancelled or superseded): no slot
		// can be resurrected for it.
		c.forget(runID, suspension)
		return fmt.Errorf("%w: parent %s: %w", engine.ErrChildCustodyRefused, runID, err)
	}
	return nil
}

// suspension returns the parent's kept suspension, or suspends it. A fresh
// park replaces any suspension kept from an earlier one.
func (c *SchedulerCapacity) suspension(ctx context.Context, runID string, park bool) (*localscheduler.ChildParentSuspension, error) {
	if c.Slots == nil || runID == "" {
		return nil, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, ErrCapacityPlaneNotComposed)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if kept, ok := c.suspended[runID]; ok && !park {
		return kept, nil
	}
	suspension, err := c.Slots.SuspendChildParent(ctx, runID)
	if err != nil {
		// No exclusive admission owner for the parent on this scheduler:
		// lending capacity it does not hold would mint a slot.
		return nil, fmt.Errorf("%w: parent %s: %w", engine.ErrChildCustodyRefused, runID, err)
	}
	if c.suspended == nil {
		c.suspended = map[string]*localscheduler.ChildParentSuspension{}
	}
	c.suspended[runID] = suspension
	return suspension, nil
}

func (c *SchedulerCapacity) forget(runID string, suspension *localscheduler.ChildParentSuspension) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.suspended[runID] == suspension {
		delete(c.suspended, runID)
	}
}
