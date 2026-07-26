package rollup

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// TimeToFirstPR returns the lifetime onboarding metric projected from journal
// identities and provider-mutation events already present in the rollup.
func (db *DB) TimeToFirstPR() (telemetry.TimeToFirstPRMetric, error) {
	var firstRun, firstPROpen sql.NullString
	if err := db.sql.QueryRow(`SELECT MIN(started_at) FROM runs`).Scan(&firstRun); err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: query first run start: %w", err)
	}
	if err := db.sql.QueryRow(`
		SELECT MIN(occurred_at)
		FROM provider_mutations
		WHERE kind = 'pr' AND operation = 'open'`).Scan(&firstPROpen); err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: query first pull request open: %w", err)
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

func parseOnboardingTime(value sql.NullString) (time.Time, error) {
	parsed, err := parseTime(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("rollup: parse time-to-first-PR timestamp: %w", err)
	}
	return parsed, nil
}
