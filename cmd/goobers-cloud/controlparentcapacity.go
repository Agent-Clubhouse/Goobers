package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/goobers/goobers/internal/cloud/childengine"
	"github.com/goobers/goobers/internal/engine"
)

// parentCapacity is the engine child protocol's ParentCapacity for the whole
// process. The scheduler is rebuilt every lease epoch, so the protocol (built
// once) reaches the CURRENT epoch's capacity through this holder, never a stale
// one. With none published (before Prepare, after Stop, standby) it refuses Park
// and Complete as not composed, the same fail-closed answer a nil capacity gives.
type parentCapacity struct {
	current atomic.Pointer[childengine.SchedulerCapacity]
}

var _ childengine.ParentCapacity = (*parentCapacity)(nil)

// epochParentCapacity is the process's holder, set by the scheduler admission
// and read by the engine child protocol.
var epochParentCapacity parentCapacity

func (c *parentCapacity) publish(capacity *childengine.SchedulerCapacity) { c.current.Store(capacity) }

// retire clears the holder only if capacity is still the published one.
func (c *parentCapacity) retire(capacity *childengine.SchedulerCapacity) {
	c.current.CompareAndSwap(capacity, nil)
}

func (c *parentCapacity) load() (*childengine.SchedulerCapacity, error) {
	capacity := c.current.Load()
	if capacity == nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, childengine.ErrCapacityPlaneNotComposed)
	}
	return capacity, nil
}

// Release lends a parked parent's slot back through the current epoch's scheduler.
func (c *parentCapacity) Release(ctx context.Context, in engine.ChildParkInput) error {
	capacity, err := c.load()
	if err != nil {
		return err
	}
	return capacity.Release(ctx, in)
}

// Reacquire takes a slot for the parent again through the current epoch's scheduler.
func (c *parentCapacity) Reacquire(ctx context.Context, in engine.ChildCompletionInput) error {
	capacity, err := c.load()
	if err != nil {
		return err
	}
	return capacity.Reacquire(ctx, in)
}
