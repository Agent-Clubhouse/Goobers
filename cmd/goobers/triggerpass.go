package main

import (
	"context"
	"sync"
	"time"
)

const triggerPassBudget = 5 * time.Second

type triggerExecutionContextKey struct{}

// Keep execution cancellation bound to the daemon lifetime while limiting the
// total sweep's provider/admission work. The private context value is host-only.
func triggerPassContext(ctx, execution context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithValue(ctx, triggerExecutionContextKey{}, execution), budget)
}

func acquireTriggerPass(ctx context.Context, mutex *sync.Mutex) error {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mutex.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
