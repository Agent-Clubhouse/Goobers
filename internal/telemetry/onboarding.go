package telemetry

import "time"

// TimeToFirstPRAnchor identifies the immutable journal field used as the
// onboarding metric's starting point.
const TimeToFirstPRAnchor = "firstRunStartedAt"

// TimeToFirstPRMetric is the lifetime onboarding interval from the first run
// recorded by an instance to its first journaled pull-request open.
type TimeToFirstPRMetric struct {
	Anchor        string     `json:"anchor"`
	FirstRunAt    *time.Time `json:"firstRunAt,omitempty"`
	FirstPROpenAt *time.Time `json:"firstPROpenAt,omitempty"`
	Milliseconds  *int64     `json:"milliseconds,omitempty"`
}

// NewTimeToFirstPRMetric builds the structured metric while preserving absent
// timestamps as absent rather than reporting a misleading zero.
func NewTimeToFirstPRMetric(firstRunAt, firstPROpenAt time.Time) TimeToFirstPRMetric {
	metric := TimeToFirstPRMetric{Anchor: TimeToFirstPRAnchor}
	if !firstRunAt.IsZero() {
		firstRunAt = firstRunAt.UTC()
		metric.FirstRunAt = &firstRunAt
	}
	if !firstPROpenAt.IsZero() {
		firstPROpenAt = firstPROpenAt.UTC()
		metric.FirstPROpenAt = &firstPROpenAt
	}
	if metric.FirstRunAt != nil && metric.FirstPROpenAt != nil {
		elapsed := metric.FirstPROpenAt.Sub(*metric.FirstRunAt)
		if elapsed < 0 {
			elapsed = 0
		}
		milliseconds := elapsed.Milliseconds()
		metric.Milliseconds = &milliseconds
	}
	return metric
}
