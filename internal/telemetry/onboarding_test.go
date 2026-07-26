package telemetry

import (
	"testing"
	"time"
)

func TestNewTimeToFirstPRMetricRejectsPROpenBeforeInitCompletion(t *testing.T) {
	initCompletedAt := time.Date(2026, time.July, 14, 12, 0, 1, 0, time.UTC)
	firstPROpenAt := initCompletedAt.Add(-time.Second)
	metric := NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt)
	if metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("metric = %#v, want pre-init PR endpoint omitted", metric)
	}
	metric = NewTimeToFirstPRMetric(time.Time{}, firstPROpenAt)
	if metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("metric without init anchor = %#v, want PR endpoint omitted", metric)
	}
}
