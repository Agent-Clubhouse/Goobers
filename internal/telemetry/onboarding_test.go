package telemetry

import (
	"testing"
	"time"
)

func TestNewTimeToFirstPRMetricClampsClockSkew(t *testing.T) {
	initCompletedAt := time.Date(2026, time.July, 14, 12, 0, 1, 0, time.UTC)
	firstPROpenAt := initCompletedAt.Add(-time.Second)
	metric := NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt)
	if metric.Milliseconds == nil || *metric.Milliseconds != 0 {
		t.Fatalf("Milliseconds = %v, want 0", metric.Milliseconds)
	}
}
