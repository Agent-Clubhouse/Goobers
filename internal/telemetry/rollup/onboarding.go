package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// TimeToFirstPR returns the lifetime first-run success metric captured from the
// instance journal and provider-mutation events during rollup ingestion.
func (db *DB) TimeToFirstPR(ctx context.Context) (telemetry.TimeToFirstPRMetric, error) {
	var initCompleted, firstPROpen sql.NullString
	if err := db.readDB().QueryRowContext(ctx, `
		SELECT init_completed_at, first_pr_open_at
		FROM first_success_milestones
		WHERE id = 1`).Scan(&initCompleted, &firstPROpen); err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: query first-success milestone: %w", err)
	}
	initCompletedAt, err := parseFirstSuccessTime(initCompleted)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	firstPROpenAt, err := parseFirstSuccessTime(firstPROpen)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	return telemetry.NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt), nil
}

func (db *DB) recordTimeToFirstPR(ctx context.Context, initCompletedAt, firstPROpenAt time.Time) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollup: begin first-success milestone tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertTimeToFirstPR(ctx, tx, initCompletedAt, firstPROpenAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit first-success milestone: %w", err)
	}
	return nil
}

func upsertTimeToFirstPR(ctx context.Context, tx *sql.Tx, initCompletedAt, firstPROpenAt time.Time) error {
	var storedInit, storedFirstPR sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT init_completed_at, first_pr_open_at
		FROM first_success_milestones
		WHERE id = 1`).Scan(&storedInit, &storedFirstPR)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rollup: read first-success milestone for update: %w", err)
	}
	currentInit, err := parseFirstSuccessTime(storedInit)
	if err != nil {
		return err
	}
	currentFirstPR, err := parseFirstSuccessTime(storedFirstPR)
	if err != nil {
		return err
	}
	initCompletedAt = earlierTime(currentInit, initCompletedAt)
	var retainedFirstPR time.Time
	if !initCompletedAt.IsZero() &&
		(currentInit.IsZero() ||
			!initCompletedAt.Equal(currentInit) ||
			(!currentFirstPR.IsZero() && currentFirstPR.Before(initCompletedAt))) {
		retainedFirstPR, err = firstRetainedPROpenAt(ctx, tx, initCompletedAt)
		if err != nil {
			return err
		}
	}
	var validFirstPROpenAt time.Time
	for _, candidate := range []time.Time{currentFirstPR, firstPROpenAt, retainedFirstPR} {
		if initCompletedAt.IsZero() || candidate.IsZero() || candidate.Before(initCompletedAt) {
			continue
		}
		validFirstPROpenAt = earlierTime(validFirstPROpenAt, candidate)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO first_success_milestones (id, init_completed_at, first_pr_open_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			init_completed_at = excluded.init_completed_at,
			first_pr_open_at = excluded.first_pr_open_at`,
		formatTime(initCompletedAt),
		formatTime(validFirstPROpenAt),
	)
	if err != nil {
		return fmt.Errorf("rollup: record first-success milestone: %w", err)
	}
	return nil
}

func firstRetainedPROpenAt(ctx context.Context, tx *sql.Tx, initCompletedAt time.Time) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT occurred_at
		FROM provider_mutations
		WHERE kind = 'pr' AND operation = 'open'`)
	if err != nil {
		return time.Time{}, fmt.Errorf("rollup: query retained PR opens for milestone: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var firstPROpenAt time.Time
	for rows.Next() {
		var value sql.NullString
		if err := rows.Scan(&value); err != nil {
			return time.Time{}, fmt.Errorf("rollup: scan retained PR open for milestone: %w", err)
		}
		candidate, err := parseFirstSuccessTime(value)
		if err != nil {
			return time.Time{}, err
		}
		if candidate.IsZero() || candidate.Before(initCompletedAt) {
			continue
		}
		firstPROpenAt = earlierTime(firstPROpenAt, candidate)
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, fmt.Errorf("rollup: iterate retained PR opens for milestone: %w", err)
	}
	return firstPROpenAt, nil
}

func earlierTime(left, right time.Time) time.Time {
	if left.IsZero() || (!right.IsZero() && right.Before(left)) {
		return right
	}
	return left
}

func runFirstPROpenAt(events []journalEvent) time.Time {
	var firstPROpenAt time.Time
	for _, event := range events {
		if event.Type != eventRefTouched ||
			event.ExternalRef == nil ||
			event.ExternalRef.Kind != "pr" ||
			operationFromRunner(event.Runner) != "open" ||
			event.Time.IsZero() {
			continue
		}
		if firstPROpenAt.IsZero() || event.Time.Before(firstPROpenAt) {
			firstPROpenAt = event.Time
		}
	}
	return firstPROpenAt
}

func parseFirstSuccessTime(value sql.NullString) (time.Time, error) {
	parsed, err := parseTime(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("rollup: parse time-to-first-PR timestamp: %w", err)
	}
	return parsed, nil
}
