package readmodel

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The retention pass (#1932, design §11.4).
//
// Runs alongside serving rather than as a maintenance window, so it is paced and
// batched: a pass that monopolised the writer would stall every read behind it,
// and one that aged out an unbounded number of runs at once would emit a single
// enormous burst of removals into the change feed — waking every connected
// client to refetch at the same moment.

// RetentionLoop applies the projection retention window on a schedule.
type RetentionLoop struct {
	store   *Store
	window  RetentionWindow
	options RetentionOptions
	stats   RetentionStats
}

// RetentionOptions configures the loop.
type RetentionOptions struct {
	// Interval is how often a pass runs.
	//
	// Deliberately slow. Retention is not latency-sensitive — a run aging out an
	// hour late is indistinguishable from one aging out on time — and every pass
	// competes with serving for the single writer.
	Interval time.Duration
	// Batch bounds one pass.
	Batch int
	// ChangeFeedKeep is how many change rows to retain.
	ChangeFeedKeep int
	Logger         *slog.Logger
}

// RetentionStats are the loop's observable counters.
type RetentionStats struct {
	Passes        int
	AgedOut       int
	ChangesPruned int64
	Failures      int
	LastPassAt    time.Time
}

const (
	defaultRetentionInterval  = time.Hour
	defaultRetentionLoopBatch = 200
)

// NewRetentionLoop constructs the loop.
func NewRetentionLoop(store *Store, window RetentionWindow, options RetentionOptions) *RetentionLoop {
	if options.Interval <= 0 {
		options.Interval = defaultRetentionInterval
	}
	if options.Batch <= 0 {
		options.Batch = defaultRetentionLoopBatch
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &RetentionLoop{store: store, window: window, options: options}
}

// Run applies retention until the context ends.
//
// # Why an unbounded window still runs the loop
//
// It does not: Run returns immediately. That matters because the alternative —
// looping and doing nothing — would burn a goroutine and a timer forever on
// every instance that has not opted in, which is all of them by default.
func (l *RetentionLoop) Run(ctx context.Context) {
	if !l.window.Bounded() {
		l.options.Logger.Debug("projection retention is unbounded; no pass will run")
		return
	}
	ticker := time.NewTicker(l.options.Interval)
	defer ticker.Stop()

	// A pass on start, so an instance that has been down past its window does
	// not wait a full interval before catching up.
	l.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.pass(ctx)
		}
	}
}

// pass runs one retention cycle.
//
// Failures are logged and counted, never fatal. Retention is housekeeping on
// derived state: a pass that fails leaves the projection larger than intended,
// which is a cost, not a correctness problem — whereas taking the daemon down
// over it would turn a cost into an outage.
func (l *RetentionLoop) pass(ctx context.Context) {
	l.stats.Passes++
	l.stats.LastPassAt = time.Now().UTC()

	result, err := l.store.ApplyRetention(ctx, l.window, l.options.Batch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			l.stats.Failures++
			l.options.Logger.Warn("projection retention pass failed", "error", err)
		}
		return
	}
	l.stats.AgedOut += result.AgedOut
	if result.AgedOut > 0 {
		l.options.Logger.Info("projection retention aged out runs",
			"aged_out", result.AgedOut, "floor", result.Floor.Format(time.RFC3339))
	}

	// Prune the change feed in the same pass, but AFTER the removals — each
	// removal writes a change row, and pruning first would leave exactly those
	// rows behind while dropping older ones a client might still be resuming
	// from.
	pruned, err := l.store.PruneChangeFeed(ctx, l.options.ChangeFeedKeep)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			l.stats.Failures++
			l.options.Logger.Warn("change feed prune failed", "error", err)
		}
		return
	}
	l.stats.ChangesPruned += pruned
}

// Stats returns a snapshot of the counters.
func (l *RetentionLoop) Stats() RetentionStats { return l.stats }
