package rollup

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// TimeToFirstPR returns the lifetime first-run success metric captured from the
// instance journal and provider-mutation events during rollup ingestion.
func (db *DB) TimeToFirstPR() (telemetry.TimeToFirstPRMetric, error) {
	var initCompleted, firstPROpen sql.NullString
	if err := db.sql.QueryRow(`
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

func (db *DB) recordTimeToFirstPR(initCompletedAt, firstPROpenAt time.Time) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("rollup: begin first-success milestone tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertTimeToFirstPR(tx, initCompletedAt, firstPROpenAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit first-success milestone: %w", err)
	}
	return nil
}

func upsertTimeToFirstPR(tx *sql.Tx, initCompletedAt, firstPROpenAt time.Time) error {
	_, err := tx.Exec(`
		INSERT INTO first_success_milestones (id, init_completed_at, first_pr_open_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			init_completed_at = CASE
				WHEN excluded.init_completed_at IS NULL THEN first_success_milestones.init_completed_at
				WHEN first_success_milestones.init_completed_at IS NULL
					OR excluded.init_completed_at < first_success_milestones.init_completed_at
					THEN excluded.init_completed_at
				ELSE first_success_milestones.init_completed_at
			END,
			first_pr_open_at = CASE
				WHEN excluded.first_pr_open_at IS NULL THEN first_success_milestones.first_pr_open_at
				WHEN first_success_milestones.first_pr_open_at IS NULL
					OR excluded.first_pr_open_at < first_success_milestones.first_pr_open_at
					THEN excluded.first_pr_open_at
				ELSE first_success_milestones.first_pr_open_at
			END`,
		formatTime(initCompletedAt),
		formatTime(firstPROpenAt),
	)
	if err != nil {
		return fmt.Errorf("rollup: record first-success milestone: %w", err)
	}
	return nil
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
