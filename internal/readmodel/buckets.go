package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Aggregate buckets (#1931, design §5.6).
//
// # Why recompute rather than accumulate
//
// When a run in day D changes, D is marked dirty and its buckets are RECOMPUTED
// by aggregating that day's indexed run rows. Bounded by runs in a day, and
// idempotent — recomputing twice produces the same rows, which serves §14.9's
// determinism property for free.
//
// Reversible deltas were considered and rejected. They require storing each
// run's prior contribution and subtracting it on reprojection: fiddly, easy to
// get wrong, and — the deciding property — it drifts SILENTLY when it is wrong.
// A recompute cannot drift, because it carries no state between passes.

// dayFormat and monthFormat are the bucket keys. Text rather than integers so a
// bucket row is legible in a database browser and sorts correctly as a string,
// which is what the recency indexes rely on.
const (
	dayFormat   = "2006-01-02"
	monthFormat = "2006-01"
)

// Bucket is one aggregated slice.
type Bucket struct {
	Key      string
	Gaggle   string
	Workflow string
	Phase    string
	Outcome  string
	Runs     int
	Duration time.Duration
}

// MarkDayDirty queues a day for recompute.
//
// Called from the projection transaction, where it is one small insert rather
// than an O(runs-in-day) aggregation — putting the recompute on the commit path
// would make every run's projection pay for its whole day.
func markDayDirty(ctx context.Context, tx txExec, at time.Time, now time.Time) error {
	if at.IsZero() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO dirty_day (day, marked_at) VALUES (?, ?)
		ON CONFLICT(day) DO NOTHING`,
		at.UTC().Format(dayFormat), formatTime(now))
	if err != nil {
		return fmt.Errorf("readmodel: mark day dirty: %w", err)
	}
	return nil
}

// txExec is the subset of *sql.Tx markDayDirty needs, so it can be called from
// inside the projection transaction without threading the whole handle.
type txExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// DirtyDays returns days awaiting recompute, oldest first.
//
// Oldest first so a backlog drains in order rather than starving early days —
// the same reason intake drains oldest-first.
func (s *Store) DirtyDays(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = defaultDirtyDayBatch
	}
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx,
		`SELECT day FROM dirty_day ORDER BY day ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read dirty days: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("readmodel: scan dirty day: %w", err)
		}
		out = append(out, day)
	}
	return out, rows.Err()
}

const defaultDirtyDayBatch = 64

// RecomputeDay rebuilds one day's buckets from the run rows.
//
// Delete-then-insert within one transaction, and the dirty marker is cleared in
// the same transaction. That ordering matters: clearing the marker first would
// lose the day entirely if the recompute then failed, and clearing it in a
// separate transaction would let a crash between the two leave a day recomputed
// but still queued — harmless, but it would recompute forever on a wedged day.
func (s *Store) RecomputeDay(ctx context.Context, day string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin bucket recompute: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM bucket_day WHERE day = ?`, day); err != nil {
		return fmt.Errorf("readmodel: clear buckets for %s: %w", day, err)
	}

	// Aggregated from the run table directly. started_at is stored as an
	// RFC3339 string, so a day is a prefix range — which the recency indexes
	// serve as a seek rather than a scan.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bucket_day (day, gaggle, workflow, phase, outcome, runs, duration_ms)
		SELECT ?, gaggle, workflow, phase, COALESCE(outcome_verdict, ''),
		       COUNT(*),
		       COALESCE(SUM(
		           CASE WHEN finished_at IS NOT NULL
		                THEN CAST((julianday(finished_at) - julianday(started_at)) * 86400000 AS INTEGER)
		                ELSE 0 END
		       ), 0)
		FROM run
		WHERE started_at >= ? AND started_at < ?
		GROUP BY gaggle, workflow, phase, COALESCE(outcome_verdict, '')`,
		day, day, dayUpperBound(day)); err != nil {
		return fmt.Errorf("readmodel: recompute buckets for %s: %w", day, err)
	}
	var nodeRows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE started_at >= ? AND started_at < ?`,
		day, dayUpperBound(day)).Scan(&nodeRows); err != nil {
		return fmt.Errorf("readmodel: count runs for node buckets %s: %w", day, err)
	}
	// Node buckets outlive individually retained run rows, just like the
	// established outcome buckets. Once a day has no source rows left, its
	// durable node projection must not be rebuilt as an empty day.
	if nodeRows > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM bucket_node_day WHERE day = ?`, day); err != nil {
			return fmt.Errorf("readmodel: clear node buckets for %s: %w", day, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bucket_node_day
				(day, gaggle, workflow, phase, outcome, kind, name, identity,
				 runs, failures, retry_waste)
			SELECT ?, r.gaggle, r.workflow, r.phase, COALESCE(r.outcome_verdict, ''),
				rn.kind, rn.name, rn.identity, COUNT(*),
				SUM(CASE WHEN r.outcome_target = '@abort'
					OR lower(r.outcome_verdict) IN ('fail', 'failure', 'reject', 'rejected')
					THEN 1 ELSE 0 END),
				SUM(rn.retry_waste_attempts)
			FROM run r JOIN run_node rn ON rn.run_id = r.run_id
			WHERE r.started_at >= ? AND r.started_at < ?
			GROUP BY r.gaggle, r.workflow, r.phase, COALESCE(r.outcome_verdict, ''),
				rn.kind, rn.name, rn.identity`,
			day, day, dayUpperBound(day)); err != nil {
			return fmt.Errorf("readmodel: recompute node buckets for %s: %w", day, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM dirty_day WHERE day = ?`, day); err != nil {
		return fmt.Errorf("readmodel: clear dirty marker for %s: %w", day, err)
	}
	return tx.Commit()
}

// dayUpperBound returns the exclusive end of a day's started_at range.
//
// String comparison against the next day's prefix, which works because the
// stored format is RFC3339 with a fixed-width date. Adding 24h to a parsed time
// would be wrong across a DST boundary in a local zone; the stored values are
// UTC, but the comparison being purely lexical means the question never arises.
func dayUpperBound(day string) string {
	parsed, err := time.Parse(dayFormat, day)
	if err != nil {
		// An unparseable day cannot match anything, which is the safe direction:
		// the recompute writes no rows rather than aggregating the whole table.
		return day
	}
	return parsed.AddDate(0, 0, 1).Format(dayFormat)
}

// RecomputeMonth rebuilds a month's rollup from the daily buckets.
//
// From the dailies rather than from run rows, so the two tiers cannot disagree:
// a month is by construction the sum of its days.
func (s *Store) RecomputeMonth(ctx context.Context, month string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin month recompute: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM bucket_month WHERE month = ?`, month); err != nil {
		return fmt.Errorf("readmodel: clear month %s: %w", month, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bucket_month (month, gaggle, workflow, phase, outcome, runs, duration_ms)
		SELECT ?, gaggle, workflow, phase, outcome, SUM(runs), SUM(duration_ms)
		FROM bucket_day
		WHERE day >= ? AND day < ?
		GROUP BY gaggle, workflow, phase, outcome`,
		month, month+"-01", monthUpperBound(month)); err != nil {
		return fmt.Errorf("readmodel: recompute month %s: %w", month, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bucket_node_month WHERE month = ?`, month); err != nil {
		return fmt.Errorf("readmodel: clear node month %s: %w", month, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bucket_node_month
			(month, gaggle, workflow, phase, outcome, kind, name, identity,
			 runs, failures, retry_waste)
		SELECT ?, gaggle, workflow, phase, outcome, kind, name, identity,
			SUM(runs), SUM(failures), SUM(retry_waste)
		FROM bucket_node_day
		WHERE day >= ? AND day < ?
		GROUP BY gaggle, workflow, phase, outcome, kind, name, identity`,
		month, month+"-01", monthUpperBound(month)); err != nil {
		return fmt.Errorf("readmodel: recompute node month %s: %w", month, err)
	}
	return tx.Commit()
}

func monthUpperBound(month string) string {
	parsed, err := time.Parse(monthFormat, month)
	if err != nil {
		return month
	}
	return parsed.AddDate(0, 1, 0).Format(monthFormat) + "-01"
}

// DayBuckets returns aggregated rows for a window.
//
// This is what makes an all-time query bounded: the row count is the number of
// (day, gaggle, workflow, phase, outcome) slices in the window, not the number
// of runs behind them.
func (s *Store) DayBuckets(ctx context.Context, gaggle string, from, to time.Time) ([]Bucket, error) {
	query := `SELECT day, gaggle, workflow, phase, outcome, runs, duration_ms
		FROM bucket_day WHERE day >= ? AND day <= ?`
	args := []any{from.UTC().Format(dayFormat), to.UTC().Format(dayFormat)}
	if gaggle != "" {
		query += ` AND gaggle = ?`
		args = append(args, gaggle)
	}
	query += ` ORDER BY day DESC, gaggle ASC, workflow ASC`

	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read day buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Bucket
	for rows.Next() {
		var (
			bucket     Bucket
			durationMS int64
		)
		if err := rows.Scan(&bucket.Key, &bucket.Gaggle, &bucket.Workflow,
			&bucket.Phase, &bucket.Outcome, &bucket.Runs, &durationMS); err != nil {
			return nil, fmt.Errorf("readmodel: scan bucket: %w", err)
		}
		bucket.Duration = time.Duration(durationMS) * time.Millisecond
		out = append(out, bucket)
	}
	return out, rows.Err()
}
