package apireadcache

import (
	"context"
	"time"
)

// StartLockSweep starts the periodic re-sweep of stale
// api-read-cache per-list-key lock files and returns the channel that closes
// once its goroutine has stopped (for the shutdown join in runUpContext).
//
// #4251: CleanStaleLocks no longer gates itself to once per
// process, so this ticker is what actually makes that removal matter for a
// daemon whose own cache-construction call sites (stage dispatch, open-PR
// polling, counter evaluation) might otherwise go quiet for longer than the
// cache's stale-lock age between calls. Same
// never-write-to-stdout footing as the other periodic sweeps in
// runUpContext; the sweep itself is fail-open with no error to report
// (CleanStaleLocks' own doc: "must never fail cache construction").
func StartLockSweep(ctx context.Context, schedulerDir string, interval time.Duration) <-chan struct{} {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				CleanStaleLocks(schedulerDir)
			}
		}
	}()
	return done
}
