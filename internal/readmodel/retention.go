package readmodel

import (
	"context"
	"fmt"
	"time"
)

// Projection retention (#1932, design §11.4, §6.3).
//
// # The window is on the PROJECTION, not on the journals
//
// The two are independent and deliberately so (§11.4). A journal is the source
// of truth and its retention is an operator's decision about disk and audit; the
// projection is derived, and its retention is a decision about what stays
// individually listable. Aging a run out of the projection removes no evidence —
// the journal is still there, and a rebuild would re-admit the run if the floor
// were lowered.
//
// # What ages out, and what does not
//
// A run older than the window stops being individually listable. It remains
// answerable IN AGGREGATE, because the buckets are computed from rows before
// they age out and are not themselves aged (§6.4).
//
// This is strictly less than the portal offers today, and it was a product
// decision rather than an engineering one: a six-month-old run stays answerable
// in aggregate but may not be individually listable.

// RetentionWindow is how much history stays individually listable.
//
// # Why unbounded is a distinct state rather than a zero
//
// The obvious encoding — a day count where 0 means "off" — is actively
// dangerous here. Compared naively, `startedAt < now - 0 days` ages out EVERY
// run immediately: the most destructive possible reading of the value an
// operator would most reasonably expect to mean "leave it alone".
//
// So unbounded short-circuits before any floor arithmetic happens, and
// RetentionWindow.Bounded() is the only way to reach that arithmetic. A zero,
// negative, or unset value all resolve here.
type RetentionWindow struct {
	days int
}

// UnboundedRetention keeps every run individually listable forever.
//
// This is the opt-out state selected by an explicit non-positive day value.
func UnboundedRetention() RetentionWindow { return RetentionWindow{days: 0} }

// RetentionDays builds a window from a configured day count.
//
// Zero, negative, and unset all mean unbounded — see the type comment. There is
// deliberately no way to express "age out everything", because no operator
// wants it and a typo should not be able to reach it.
func RetentionDays(days int) RetentionWindow {
	if days <= 0 {
		return UnboundedRetention()
	}
	return RetentionWindow{days: days}
}

// Bounded reports whether this window ages anything out.
func (w RetentionWindow) Bounded() bool { return w.days > 0 }

// Days reports the configured window, or 0 when unbounded.
func (w RetentionWindow) Days() int { return w.days }

// FloorAt returns the projection floor this window implies at a given time.
//
// Only meaningful when Bounded. Callers must check first; this returns the zero
// time otherwise, which is a floor no run can be below — the safe direction if
// someone skips the check.
func (w RetentionWindow) FloorAt(now time.Time) time.Time {
	if !w.Bounded() {
		return time.Time{}
	}
	return now.AddDate(0, 0, -w.days).UTC()
}

// String renders the window for logs and diagnostics.
func (w RetentionWindow) String() string {
	if !w.Bounded() {
		return "unbounded"
	}
	return fmt.Sprintf("%dd", w.days)
}

// RetentionResult reports what one pass aged out.
type RetentionResult struct {
	Floor      time.Time
	AgedOut    int
	Tombstoned int
	// Skipped counts runs above the floor that were examined and kept. Reported
	// so a pass that aged out nothing is distinguishable from one that ran
	// against an empty store.
	Skipped int
}

// RetentionWriter routes retention mutations through the read model's sole writer.
type RetentionWriter interface {
	Tombstone(ctx context.Context, runID string, startedAt time.Time, reason string) error
	RemoveRun(ctx context.Context, runID string) error
	SetProjectionFloor(ctx context.Context, floor time.Time) error
	PruneChangeFeed(ctx context.Context, keep int) (int64, error)
}

// ApplyRetention ages runs out of the projection down to the window's floor.
//
// # Ordering
//
// Tombstone, then remove. A tombstone with no removal is a harmless duplicate on
// the next pass; a removal with no tombstone is a run the repair sweep will
// re-admit from its journal, retention will delete again, and the cycle repeats
// — consuming the sweep's whole budget and flooding the change feed. That
// livelock is the reason tombstones exist (§6.3).
//
// The floor advances LAST, after the rows are gone. If it advanced first and the
// pass then failed, the floor would exclude runs that are still present, and
// repair would skip them forever as though they had been aged out.
func (s *Store) ApplyRetention(
	ctx context.Context,
	writer RetentionWriter,
	window RetentionWindow,
	limit int,
) (RetentionResult, error) {
	var result RetentionResult
	if !window.Bounded() {
		// Unbounded: no floor, no tombstones, no removals. Returning early here
		// rather than computing a zero floor is what keeps the dangerous
		// arithmetic unreachable.
		return result, nil
	}
	if limit <= 0 {
		limit = defaultRetentionBatch
	}

	floor := window.FloorAt(s.now())
	result.Floor = floor

	rows, err := s.ProjectedRunIDsBefore(ctx, floor, limit)
	if err != nil {
		return result, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !row.StartedAt.Before(floor) {
			result.Skipped++
			continue
		}
		if err := writer.Tombstone(ctx, row.RunID, row.StartedAt, "retention_window"); err != nil {
			return result, err
		}
		result.Tombstoned++
		if err := writer.RemoveRun(ctx, row.RunID); err != nil {
			return result, err
		}
		result.AgedOut++
	}

	// Only advance the floor once this batch is actually gone.
	if err := writer.SetProjectionFloor(ctx, floor); err != nil {
		return result, err
	}
	return result, nil
}

// defaultRetentionBatch bounds one pass.
//
// Retention runs alongside serving, so a single pass must not monopolise the
// writer. A large backlog drains across several passes, which is also what keeps
// the change feed's removal burst bounded.
const defaultRetentionBatch = 200

// PruneChangeFeed drops change rows the retention window no longer needs.
//
// This is #1919's outstanding acceptance criterion, transferred here: the
// mechanism existed but nothing called it, so the feed grew without bound.
//
// Changes are pruned to the OLDEST position any live client could still be
// resuming from, which is bounded by the feed's own retention rather than by the
// run window: a client that has been connected for an hour holds a cursor far
// newer than a 90-day floor, and pruning to the run floor would be both
// unnecessary and slower.
func (s *Store) PruneChangeFeed(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = defaultChangeRetention
	}
	latest, err := s.LatestChangeSeq(ctx)
	if err != nil {
		return 0, err
	}
	if latest <= uint64(keep) {
		return 0, nil
	}
	return s.PruneChanges(ctx, latest-uint64(keep)+1)
}

// defaultChangeRetention is how many change rows are kept.
//
// Sized so a client can be disconnected for a long while and still resume: at a
// busy instance's rate this is hours of history. A client past it gets
// `feed_truncated` — a named condition it can act on (§8.2) — rather than a
// silent gap.
const defaultChangeRetention = 50_000
