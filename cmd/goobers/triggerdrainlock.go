package main

import (
	"context"
	"sync"
)

// drainGuard serializes the daemon's sweep cursors without making a cancelled
// caller wait for the active owner's provider or child-runtime work. Its zero
// value is ready for use. Cancellation of a waiter never cancels the owner.
type drainGuard struct {
	once sync.Once
	held chan struct{}
}

func (g *drainGuard) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.once.Do(func() { g.held = make(chan struct{}, 1) })
	select {
	case g.held <- struct{}{}:
		release := func() { <-g.held }
		// Both select branches may be ready. Do not begin queue work when the
		// caller was already cancelled at acquisition.
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
