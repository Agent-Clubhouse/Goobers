package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
)

func TestSelfExecutionMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})
	client.SelfExecutionPolicy(true)
	client.SelfExecutionObserved(true)
	collected := collectMetrics(t, reader)
	for name, want := range map[string]float64{MetricSelfDenied: 1, MetricSelfPlacements: 0, MetricSelfRefusals: 1} {
		points := metricPoints(t, collected, name)
		if len(points) != 1 || points[0].value != want {
			t.Fatalf("%s: %+v", name, points)
		}
	}
}
