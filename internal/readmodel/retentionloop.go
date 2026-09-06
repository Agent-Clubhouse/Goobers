package readmodel

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The retention pass (#1932, design §11.4).
//
// Runs alongside serving rather than as a maintenance window, so it is paced and
// batched: a pass that monopolised the writer would stall every read behind it,
// and one that aged out an unbounded number of runs at once would emit a single
// enormous burst of removals into the change feed — waking every connected
// client to refetch at the same moment.

// RetentionLoop applies projection and change-feed retention on a schedule.
type RetentionLoop struct {
	store   *Store
	writer  RetentionWriter
	window  RetentionWindow
	options RetentionOptions
	mu      sync.RWMutex
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
	Passes          int
	AgedOut         int
	ChangesPruned   int64
	Failures        int
	LastPassAt      time.Time
	State           string
	Kind            string
	Trigger         string
	StartedAt       time.Time
	LastProgressAt  time.Time
	CurrentPhase    string
	Candidates      int
	Removed         int
	Failed          int
	LastCompletedAt time.Time
	LastResult      string
	LastError       string
}

const (
	defaultRetentionInterval  = time.Hour
	defaultRetentionLoopBatch = 200
)

// NewRetentionLoop constructs the loop.
func NewRetentionLoop(
	store *Store,
	writer RetentionWriter,
	window RetentionWindow,
	options RetentionOptions,
) *RetentionLoop {
	if options.Interval <= 0 {
		options.Interval = defaultRetentionInterval
	}
	if options.Batch <= 0 {
		options.Batch = defaultRetentionLoopBatch
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &RetentionLoop{
		store:   store,
		writer:  writer,
		window:  window,
		options: options,
		stats:   RetentionStats{State: "none", Kind: "retention-sweep"},
	}
}

// Run applies retention until the context ends.
func (l *RetentionLoop) Run(ctx context.Context) {
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
	startedAt := time.Now().UTC()
	l.mu.Lock()
	l.stats.Passes++
	l.stats.LastPassAt = time.Now().UTC()
	l.stats.State = "running"
	l.stats.Kind = "retention-sweep"
	l.stats.Trigger = "periodic"
	if l.stats.Passes == 1 {
		l.stats.Trigger = "startup"
	}
	l.stats.StartedAt = startedAt
	l.stats.LastProgressAt = startedAt
	l.stats.CurrentPhase = "projection-retention"
	l.stats.Candidates = 0
	l.stats.Removed = 0
	l.stats.Failed = 0
	l.stats.LastError = ""
	l.mu.Unlock()

	if l.window.Bounded() {
		result, err := l.store.ApplyRetention(ctx, l.writer, l.window, l.options.Batch)
		if err != nil {
			l.finish(ctx, startedAt, err)
			if !errors.Is(err, context.Canceled) {
				l.mu.Lock()
				l.stats.Failures++
				l.stats.Failed++
				l.mu.Unlock()
				l.options.Logger.Warn("projection retention pass failed", "error", err)
			}
			return
		}
		l.mu.Lock()
		l.stats.AgedOut += result.AgedOut
		l.stats.Candidates = result.AgedOut + result.Skipped
		l.stats.Removed = result.AgedOut
		l.stats.LastProgressAt = time.Now().UTC()
		l.mu.Unlock()
		if result.AgedOut > 0 {
			l.options.Logger.Info("projection retention aged out runs",
				"aged_out", result.AgedOut, "floor", result.Floor.Format(time.RFC3339))
		}
	}

	// Prune the change feed in the same pass, but AFTER the removals — each
	// removal writes a change row, and pruning first would leave exactly those
	// rows behind while dropping older ones a client might still be resuming
	// from.
	pruned, err := l.writer.PruneChangeFeed(ctx, l.options.ChangeFeedKeep)
	if err != nil {
		l.finish(ctx, startedAt, err)
		if !errors.Is(err, context.Canceled) {
			l.mu.Lock()
			l.stats.Failures++
			l.stats.Failed++
			l.mu.Unlock()
			l.options.Logger.Warn("change feed prune failed", "error", err)
		}
		return
	}
	l.mu.Lock()
	l.stats.ChangesPruned += pruned
	l.mu.Unlock()
	l.finish(ctx, startedAt, nil)
}

func (l *RetentionLoop) finish(ctx context.Context, startedAt time.Time, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats.LastProgressAt = time.Now().UTC()
	l.stats.LastCompletedAt = l.stats.LastProgressAt
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		l.stats.State = "cancelled"
		l.stats.LastResult = "cancelled"
	case err != nil:
		l.stats.State = "failed"
		l.stats.LastResult = "failed"
		l.stats.LastError = sanitizeRetentionError(err)
	default:
		l.stats.State = "completed"
		l.stats.LastResult = "completed"
	}
	l.stats.StartedAt = startedAt
}

func sanitizeRetentionError(err error) string {
	const maxErrorLength = 256
	message := err.Error()
	if len(message) > maxErrorLength {
		return message[:maxErrorLength]
	}
	return message
}

// Stats returns a snapshot of the counters.
func (l *RetentionLoop) Stats() RetentionStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.stats
}
