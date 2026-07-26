package rollup

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// TimeToFirstPR returns the lifetime onboarding metric captured from journal
// identities and provider-mutation events during rollup ingestion.
func (db *DB) TimeToFirstPR() (telemetry.TimeToFirstPRMetric, error) {
	var firstRun, firstPROpen sql.NullString
	if err := db.sql.QueryRow(`
		SELECT first_run_at, first_pr_open_at
		FROM onboarding_milestones
		WHERE id = 1`).Scan(&firstRun, &firstPROpen); err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: query onboarding milestone: %w", err)
	}
	firstRunAt, err := parseOnboardingTime(firstRun)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	firstPROpenAt, err := parseOnboardingTime(firstPROpen)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	return telemetry.NewTimeToFirstPRMetric(firstRunAt, firstPROpenAt), nil
}

func (db *DB) recordTimeToFirstPR(firstRunAt, firstPROpenAt time.Time) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("rollup: begin onboarding milestone tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertTimeToFirstPR(tx, firstRunAt, firstPROpenAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit onboarding milestone: %w", err)
	}
	return nil
}

func upsertTimeToFirstPR(tx *sql.Tx, firstRunAt, firstPROpenAt time.Time) error {
	_, err := tx.Exec(`
		INSERT INTO onboarding_milestones (id, first_run_at, first_pr_open_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			first_run_at = CASE
				WHEN excluded.first_run_at IS NULL THEN onboarding_milestones.first_run_at
				WHEN onboarding_milestones.first_run_at IS NULL
					OR excluded.first_run_at < onboarding_milestones.first_run_at
					THEN excluded.first_run_at
				ELSE onboarding_milestones.first_run_at
			END,
			first_pr_open_at = CASE
				WHEN excluded.first_pr_open_at IS NULL THEN onboarding_milestones.first_pr_open_at
				WHEN onboarding_milestones.first_pr_open_at IS NULL
					OR excluded.first_pr_open_at < onboarding_milestones.first_pr_open_at
					THEN excluded.first_pr_open_at
				ELSE onboarding_milestones.first_pr_open_at
			END`,
		formatTime(firstRunAt),
		formatTime(firstPROpenAt),
	)
	if err != nil {
		return fmt.Errorf("rollup: record onboarding milestone: %w", err)
	}
	return nil
}

func runTimeToFirstPR(identity runIdentity, events []journalEvent) (time.Time, time.Time) {
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
	return identity.StartedAt, firstPROpenAt
}

func parseOnboardingTime(value sql.NullString) (time.Time, error) {
	parsed, err := parseTime(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("rollup: parse time-to-first-PR timestamp: %w", err)
	}
	return parsed, nil
}
