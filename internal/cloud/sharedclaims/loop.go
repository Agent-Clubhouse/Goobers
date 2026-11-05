package sharedclaims

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
)

// WithReconciler attaches label repair to the epoch's awaiter lifecycle. The
// independent loop cannot delay terminal delivery or claim renewal. Reattach's
// context is the epoch lifetime; Stop cancels and joins the loop before release.
func WithReconciler(awaiters epoch.Awaiters, sweep func(context.Context) error, report func(error)) epoch.Awaiters {
	return &visibilityAwaiters{Awaiters: awaiters, sweep: sweep, report: report}
}

type visibilityAwaiters struct {
	epoch.Awaiters
	sweep  func(context.Context) error
	report func(error)
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
}

func (a *visibilityAwaiters) Reattach(ctx context.Context, handle *lease.Handle, open map[string]bool) error {
	if err := a.Awaiters.Reattach(ctx, handle, open); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.cancel != nil {
		return nil
	}
	ctx, a.cancel = context.WithCancel(ctx)
	a.done = make(chan struct{})
	go a.run(ctx, a.done)
	return nil
}

func (a *visibilityAwaiters) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()
	for ctx.Err() == nil {
		if err := a.sweep(ctx); err != nil && ctx.Err() == nil && a.report != nil {
			a.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (a *visibilityAwaiters) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	if a.cancel != nil {
		a.cancel()
	}
	done := a.done
	a.mu.Unlock()
	err := a.Awaiters.Stop(ctx)
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}
