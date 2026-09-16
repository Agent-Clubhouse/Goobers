package telemetry

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestMetricContractMatchesCommittedArtifact(t *testing.T) {
	want, err := os.ReadFile(metricContractPath)
	if err != nil {
		t.Fatalf("read %s: %v", metricContractPath, err)
	}
	got, err := telemetryMetricContractJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale.\n--- got ---\n%s\n--- want ---\n%s", metricContractPath, got, want)
	}
}

func TestMetricContractMatchesRuntimeEmission(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	reader := metric.NewManualReader()
	registry, scrubber := journal.DefaultScrubber()
	const registeredSecret = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"
	registry.Register([]byte(registeredSecret))

	client := newMetricsClient(t, Config{
		MetricReader:   reader,
		Scrubber:       scrubber,
		ServiceVersion: "v1.2.3",
		BuildCommit:    "abc1234",
		Environment:    "production",
	})

	runCtx, runSpan, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}

	stageDir := t.TempDir()
	telemetryDir := PrepareStageTelemetryDir(stageDir)
	if telemetryDir == "" {
		t.Fatal("PrepareStageTelemetryDir returned an empty path")
	}
	if err := os.WriteFile(filepath.Join(telemetryDir, metricsFile), []byte("{\"name\":\"files.changed\",\"value\":3,\"unit\":\"{file}\"}\n"), 0o600); err != nil {
		t.Fatalf("write stage metrics: %v", err)
	}

	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle:      "acme-web",
		WorkflowID:  "implement",
		RunID:       testRunID,
		TaskID:      "build",
		TaskType:    StageTypeAgentic,
		Model:       "gpt-5.4",
		Attempt:     2,
		AttemptKind: AttemptKindPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	IngestStageEmissions(telemetryDir, nil, taskSpan)
	taskSpan.CompleteWithError(OutcomeFailure, "BUILD_FAILED", true)

	_, escalatedTask, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle:      "acme-web",
		WorkflowID:  "implement",
		RunID:       testRunID,
		TaskID:      "handoff",
		TaskType:    StageTypeAgentic,
		Model:       "gpt-5.4",
		Attempt:     2,
		AttemptKind: AttemptKindInfra,
	})
	if err != nil {
		t.Fatal(err)
	}
	escalatedTask.Complete("escalated", false)

	_, gateSpan, err := client.StartGate(runCtx, GateAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "implement",
		RunID:      testRunID,
		GateID:     "review",
		Agentic:    true,
		Model:      "gpt-5.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	gateSpan.SetGateResult("escalated", 1)
	gateSpan.Complete("escalated", false)

	_, schedulerSpan, err := client.StartSchedulerSpan(context.Background(), SchedulerAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "implement",
		Action:     "claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerSpan.Succeed("claimed")

	client.InstanceJournalAppendDropped()
	client.SnapshotCaptured("delta", 123)
	client.SnapshotCaptured("full", 456)
	client.SnapshotFallback("no_base_ref")
	client.SnapshotRestoreFailed("base_missing")
	client.StorageHealthSampled("warning", 2048, true)
	client.RecordWorkcopyUsage(context.Background(), worktree.UsageMeasurement{
		Operation:        worktree.UsageOperationCreate,
		Gaggle:           "acme-web",
		OwnerRunID:       testRunID,
		WorktreeID:       "wt-1",
		WorktreeBytes:    512,
		WorktreeMeasured: true,
		WorkcopyBytes:    1024,
		WorkcopyMeasured: true,
	})

	if got := scrubber.Scrub([]byte("known value: " + registeredSecret)); !bytes.Contains(got, []byte(RedactedPlaceholder)) {
		t.Fatalf("registered scrub did not redact: %q", got)
	}
	const patternedSecret = "ghp_0123456789abcdefghijklmnopqrstuvwxyzA"
	if got := scrubber.Scrub([]byte("shaped value: " + patternedSecret)); !bytes.Contains(got, []byte(RedactedPlaceholder)) {
		t.Fatalf("pattern scrub did not redact: %q", got)
	}

	runSpan.CompleteWithError(OutcomeFailure, "RUN_FAILED", true)

	collected := collectMetrics(t, reader)
	assertRuntimeMetricsMatchContract(t, collected, telemetryMetricContract())
}

func TestMetricContractResourceAttributesMatchRuntime(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	contract := telemetryMetricContract()

	configuredReader := metric.NewManualReader()
	configuredClient := newMetricsClient(t, Config{
		MetricReader:   configuredReader,
		ServiceVersion: "v1.2.3",
		BuildCommit:    "abc1234",
		Environment:    "production",
	})
	_, span, err := configuredClient.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Succeed("configured")
	configuredAttrs := resourceAttributesFromMetrics(collectMetrics(t, configuredReader))

	defaultReader := metric.NewManualReader()
	defaultClient := newMetricsClient(t, Config{MetricReader: defaultReader})
	_, defaultSpan, err := defaultClient.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: "1af7651916cd43dd8448eb211c80319d",
	})
	if err != nil {
		t.Fatal(err)
	}
	defaultSpan.Succeed("default")
	defaultAttrs := resourceAttributesFromMetrics(collectMetrics(t, defaultReader))

	for _, spec := range contract.ResourceAttributes {
		switch spec.Presence {
		case resourcePresenceRequired:
			if configuredAttrs[spec.Name] == "" {
				t.Fatalf("required resource attribute %s missing from configured client", spec.Name)
			}
			if defaultAttrs[spec.Name] == "" {
				t.Fatalf("required resource attribute %s missing from default client", spec.Name)
			}
		case resourcePresenceOptional:
			if configuredAttrs[spec.Name] == "" {
				t.Fatalf("optional resource attribute %s missing when configured", spec.Name)
			}
			if defaultAttrs[spec.Name] != "" {
				t.Fatalf("optional resource attribute %s = %q for default client, want omitted", spec.Name, defaultAttrs[spec.Name])
			}
		default:
			t.Fatalf("unexpected resource attribute presence %q for %s", spec.Presence, spec.Name)
		}
	}
}

func assertRuntimeMetricsMatchContract(t *testing.T, collected metricdata.ResourceMetrics, contract metricContractDocument) {
	t.Helper()

	observed := observedMetricDimensions(collected)
	if len(observed) != len(contract.Metrics) {
		t.Fatalf("observed %d first-class metrics, contract lists %d", len(observed), len(contract.Metrics))
	}

	for _, spec := range contract.Metrics {
		dims, ok := observed[spec.Name]
		if !ok {
			t.Fatalf("contract metric %s was not emitted by the runtime fixture", spec.Name)
		}
		want := append([]string(nil), spec.AllowedDimensions...)
		sort.Strings(want)
		if !equalStrings(dims, want) {
			t.Fatalf("metric %s dimensions = %v, want %v", spec.Name, dims, want)
		}
	}
}

func observedMetricDimensions(collected metricdata.ResourceMetrics) map[string][]string {
	keysByMetric := make(map[string]map[string]struct{})
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if _, ok := keysByMetric[m.Name]; !ok {
				keysByMetric[m.Name] = make(map[string]struct{})
			}
			for _, point := range dataPointsForContract(m) {
				for _, attr := range point {
					keysByMetric[m.Name][string(attr.Key)] = struct{}{}
				}
			}
		}
	}

	result := make(map[string][]string, len(keysByMetric))
	for name, keys := range keysByMetric {
		result[name] = sortedKeys(keys)
	}
	return result
}

func dataPointsForContract(m metricdata.Metrics) [][]attribute.KeyValue {
	var points [][]attribute.KeyValue
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			points = append(points, point.Attributes.ToSlice())
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			points = append(points, point.Attributes.ToSlice())
		}
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			points = append(points, point.Attributes.ToSlice())
		}
	default:
		panic("unsupported metric data type in contract test")
	}
	return points
}

func resourceAttributesFromMetrics(collected metricdata.ResourceMetrics) map[string]string {
	attrs := make(map[string]string)
	if collected.Resource == nil {
		return attrs
	}
	for _, attr := range collected.Resource.Attributes() {
		attrs[string(attr.Key)] = attr.Value.String()
	}
	return attrs
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
