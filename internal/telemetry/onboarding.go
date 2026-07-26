package telemetry

import "time"

// TimeToFirstPRAnchor identifies the successful-init journal timestamp used as
// the first-run success interval's starting point.
const TimeToFirstPRAnchor = "initCompletedAt"

// TimeToFirstPRMetric is the lifetime first-run success interval from successful
// instance initialization to its first journaled pull-request open.
type TimeToFirstPRMetric struct {
	Anchor          string     `json:"anchor"`
	InitCompletedAt *time.Time `json:"initCompletedAt,omitempty"`
	FirstPROpenAt   *time.Time `json:"firstPROpenAt,omitempty"`
	Milliseconds    *int64     `json:"milliseconds,omitempty"`
}

// NewTimeToFirstPRMetric builds the structured metric while preserving absent
// timestamps as absent rather than reporting a misleading zero.
func NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt time.Time) TimeToFirstPRMetric {
	metric := TimeToFirstPRMetric{Anchor: TimeToFirstPRAnchor}
	if !initCompletedAt.IsZero() {
		initCompletedAt = initCompletedAt.UTC()
		metric.InitCompletedAt = &initCompletedAt
	}
	if !firstPROpenAt.IsZero() {
		firstPROpenAt = firstPROpenAt.UTC()
		metric.FirstPROpenAt = &firstPROpenAt
	}
	if metric.InitCompletedAt != nil && metric.FirstPROpenAt != nil {
		elapsed := metric.FirstPROpenAt.Sub(*metric.InitCompletedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		milliseconds := elapsed.Milliseconds()
		metric.Milliseconds = &milliseconds
	}
	return metric
}
