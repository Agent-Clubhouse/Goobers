package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/worktree"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"
)

const metricCompatibilityFixturePath = "testdata/metric_compatibility_fixtures.json"

func TestOTLPMetricTemporalityAndTerminalCompatibility(t *testing.T) {
	scenario := buildAllMetricsCompatibilityScenario(t)
	if len(scenario.Exports) != 1 {
		t.Fatalf("%s exported %d requests, want 1", scenario.Name, len(scenario.Exports))
	}
	req := scenario.Exports[0]

	for name, want := range map[string]struct {
		kind        string
		temporality metricspb.AggregationTemporality
		monotonic   bool
	}{
		MetricRunDuration:              {kind: "histogram", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE},
		MetricRunOutcomes:              {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricStageDuration:            {kind: "histogram", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE},
		MetricStageOutcomes:            {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricStageRetries:             {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricGateDecisions:            {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricEscalations:              {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricRedactionsTotal:          {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricJournalAppendsDropped:    {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricWorkActive:               {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: false},
		MetricStageMetricValue:         {kind: "histogram", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE},
		MetricRecoverySnapshotFormat:   {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricRecoverySnapshotBytes:    {kind: "histogram", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE},
		MetricRecoverySnapshotFallback: {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricRecoveryRestoreFailures:  {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
		MetricStorageHealthTierChanges: {kind: "sum", temporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, monotonic: true},
	} {
		assertMetricKindAndTemporality(t, req, name, want.kind, want.temporality, want.monotonic)
	}
	for _, name := range []string{EventWorktreeDiskUsage, EventWorkcopyDiskUsage, MetricStorageFreeBytes} {
		assertGaugeMetric(t, req, name)
	}

	runOutcomes := findNumberPoint(t, metricByName(t, req, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	if runOutcomes.AsInt != 1 {
		t.Fatalf("%s success point = %d, want 1", MetricRunOutcomes, runOutcomes.AsInt)
	}

	successStage := findNumberPoint(t, metricByName(t, req, MetricStageOutcomes), map[string]string{
		AttrStage:   "success-task",
		AttrOutcome: OutcomeSuccess,
	})
	if successStage.AsInt != 1 {
		t.Fatalf("%s success-task = %d, want 1 even after Complete+End", MetricStageOutcomes, successStage.AsInt)
	}
	failedStage := findNumberPoint(t, metricByName(t, req, MetricStageOutcomes), map[string]string{
		AttrStage:   "failed-task",
		AttrOutcome: OutcomeFailure,
	})
	if failedStage.AsInt != 1 {
		t.Fatalf("%s failed-task = %d, want 1", MetricStageOutcomes, failedStage.AsInt)
	}
	cancelledStage := findNumberPoint(t, metricByName(t, req, MetricStageOutcomes), map[string]string{
		AttrStage:   "cancelled-task",
		AttrOutcome: OutcomeBlocked,
	})
	if cancelledStage.AsInt != 1 {
		t.Fatalf("%s cancelled-task = %d, want 1", MetricStageOutcomes, cancelledStage.AsInt)
	}
	shutdownStage := findNumberPoint(t, metricByName(t, req, MetricStageOutcomes), map[string]string{
		AttrStage:   "shutdown",
		AttrOutcome: outcomeUnset,
	})
	if shutdownStage.AsInt != 1 {
		t.Fatalf("%s shutdown = %d, want 1 deferred End path", MetricStageOutcomes, shutdownStage.AsInt)
	}

	for _, subset := range []map[string]string{
		{AttrWorkflow: "compatibility-workflow", MetricAttrSpanKind: SpanKindRun},
		{AttrWorkflow: "compatibility-workflow", MetricAttrSpanKind: SpanKindTask},
		{AttrWorkflow: "compatibility-workflow", MetricAttrSpanKind: SpanKindGate},
		{AttrWorkflow: "compatibility-workflow", MetricAttrSpanKind: SpanKindScheduler},
	} {
		point := findNumberPoint(t, metricByName(t, req, MetricWorkActive), subset)
		if point.AsInt != 0 {
			t.Fatalf("%s%v = %d, want 0", MetricWorkActive, subset, point.AsInt)
		}
		if point.AsInt < 0 {
			t.Fatalf("%s%v = %d, want non-negative", MetricWorkActive, subset, point.AsInt)
		}
	}
}

func TestOTLPMetricForceFlushThenPeriodicCollectionCompatibility(t *testing.T) {
	scenario := buildForceFlushThenPeriodicScenario(t)
	if len(scenario.Exports) < 2 {
		t.Fatalf("%s exported %d requests, want at least 2", scenario.Name, len(scenario.Exports))
	}
	first := scenario.Exports[0]
	second := scenario.Exports[1]

	firstPoint := findNumberPoint(t, metricByName(t, first, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	secondPoint := findNumberPoint(t, metricByName(t, second, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	if firstPoint.AsInt != 1 || secondPoint.AsInt != 1 {
		t.Fatalf("%s values after flush/periodic = %d/%d, want 1/1", MetricRunOutcomes, firstPoint.AsInt, secondPoint.AsInt)
	}
	if got, want := resourceAttributeMap(first)["service.instance.id"], resourceAttributeMap(second)["service.instance.id"]; got != want {
		t.Fatalf("service.instance.id changed across flush/periodic: %q vs %q", got, want)
	}
}

func TestOTLPMetricDaemonRestartStartsNewCumulativeStream(t *testing.T) {
	scenario := buildDaemonRestartScenario(t)
	if len(scenario.Exports) != 2 {
		t.Fatalf("%s exported %d requests, want 2", scenario.Name, len(scenario.Exports))
	}
	first := scenario.Exports[0]
	second := scenario.Exports[1]

	firstResource := resourceAttributeMap(first)
	secondResource := resourceAttributeMap(second)
	if firstResource["service.instance.id"] == secondResource["service.instance.id"] {
		t.Fatalf("daemon restart reused service.instance.id %q", firstResource["service.instance.id"])
	}
	delete(firstResource, "service.instance.id")
	delete(secondResource, "service.instance.id")
	if !mapsEqual(firstResource, secondResource) {
		t.Fatalf("resource identity changed across restart beyond service.instance.id:\nfirst:  %v\nsecond: %v", firstResource, secondResource)
	}

	gateway := newCumulativeDeltaGateway()
	firstDelta := gateway.observeCounter(t, first, MetricRunOutcomes, map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	secondDelta := gateway.observeCounter(t, second, MetricRunOutcomes, map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	if firstDelta != 1 || secondDelta != 1 {
		t.Fatalf("cumulative-to-delta run outcomes = %d/%d, want 1/1 across restart", firstDelta, secondDelta)
	}

	firstCount, firstSum := gateway.observeHistogram(t, first, MetricRunDuration, map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	secondCount, secondSum := gateway.observeHistogram(t, second, MetricRunDuration, map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	if firstCount != 1 || secondCount != 1 || firstSum != 5 || secondSum != 7 {
		t.Fatalf("cumulative-to-delta run durations = count/sum %d/%.0f then %d/%.0f, want 1/5 then 1/7",
			firstCount, firstSum, secondCount, secondSum)
	}
}

func TestOTLPMetricCollectorRestartRecovers(t *testing.T) {
	scenario := buildCollectorRestartScenario(t)
	if len(scenario.Exports) != 3 {
		t.Fatalf("%s exported %d requests, want 3", scenario.Name, len(scenario.Exports))
	}
	first := scenario.Exports[0]
	second := scenario.Exports[1]
	third := scenario.Exports[2]

	firstPoint := findNumberPoint(t, metricByName(t, first, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	secondPoint := findNumberPoint(t, metricByName(t, second, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	thirdPoint := findNumberPoint(t, metricByName(t, third, MetricRunOutcomes), map[string]string{
		AttrWorkflow: "compatibility-workflow",
		AttrOutcome:  OutcomeSuccess,
	})
	if firstPoint.AsInt != 1 {
		t.Fatalf("first collector export run count = %d, want 1", firstPoint.AsInt)
	}
	if secondPoint.AsInt != 2 {
		t.Fatalf("post-restart collector export run count = %d, want 2 from the first successful export after recovery", secondPoint.AsInt)
	}
	if thirdPoint.AsInt != 3 {
		t.Fatalf("post-restart collector export after new work = %d, want 3", thirdPoint.AsInt)
	}
	for i, req := range []*collectormetrics.ExportMetricsServiceRequest{second, third} {
		if got, want := resourceAttributeMap(first)["service.instance.id"], resourceAttributeMap(req)["service.instance.id"]; got != want {
			t.Fatalf("collector restart changed daemon service.instance.id on export %d: %q vs %q", i+2, got, want)
		}
	}
}

func TestOTLPMetricCompatibilityFixturesStayCurrent(t *testing.T) {
	fixtures := metricCompatibilityFixtureFile{
		Schema: "goobers.dev/telemetry/metric-compatibility-fixtures/v1",
		Scenarios: []metricCompatibilityScenario{
			buildAllMetricsCompatibilityScenario(t),
			buildForceFlushThenPeriodicScenario(t),
			buildDaemonRestartScenario(t),
			buildCollectorRestartScenario(t),
		},
	}
	got, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatalf("marshal metric compatibility fixtures: %v", err)
	}
	got = append(got, '\n')

	path := filepath.FromSlash(metricCompatibilityFixturePath)
	if os.Getenv("GOOBERS_UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale; re-run with GOOBERS_UPDATE_FIXTURES=1", path)
	}
}

type metricCompatibilityFixtureFile struct {
	Schema    string                        `json:"schema"`
	Scenarios []metricCompatibilityScenario `json:"scenarios"`
}

type metricCompatibilityScenario struct {
	Name        string                                          `json:"name"`
	Description string                                          `json:"description"`
	Requests    []json.RawMessage                               `json:"requests"`
	Exports     []*collectormetrics.ExportMetricsServiceRequest `json:"-"`
}

func buildAllMetricsCompatibilityScenario(t *testing.T) metricCompatibilityScenario {
	t.Helper()

	collector := newMetricCompatibilityCollectorServer(t)
	client := newCompatibilityClient(t, collector.endpoint(), "daemon-all-metrics", time.Hour)

	base := time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)
	runCtx, runSpan, err := client.StartRun(context.Background(), RunAttributes{
		StartedAt:  base,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "11111111111111111111111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, successTask, err := client.StartTask(runCtx, TaskAttributes{
		StartedAt:   base.Add(time.Second),
		Gaggle:      "acme-web",
		WorkflowID:  "compatibility-workflow",
		RunID:       "11111111111111111111111111111111",
		TaskID:      "success-task",
		TaskType:    StageTypeAgentic,
		Model:       "gpt-5.4",
		Attempt:     2,
		AttemptKind: AttemptKindPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	stageDir := PrepareStageTelemetryDir(t.TempDir())
	if err := os.WriteFile(filepath.Join(stageDir, metricsFile), []byte(`{"name":"files.changed","value":4,"unit":"{file}"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var result apiv1.ResultEnvelope
	IngestStageEmissions(stageDir, &result, successTask)
	completeSpanAt(successTask, base.Add(3*time.Second), OutcomeSuccess, "", false)
	successTask.End()

	_, failedTask, err := client.StartTask(runCtx, TaskAttributes{
		StartedAt:  base.Add(4 * time.Second),
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "11111111111111111111111111111111",
		TaskID:     "failed-task",
		TaskType:   StageTypeDeterministic,
	})
	if err != nil {
		t.Fatal(err)
	}
	failSpanAt(failedTask, base.Add(6*time.Second), errors.New("fixture failed"), "")

	_, cancelledTask, err := client.StartTask(runCtx, TaskAttributes{
		StartedAt:  base.Add(7 * time.Second),
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "11111111111111111111111111111111",
		TaskID:     "cancelled-task",
		TaskType:   StageTypeDeterministic,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(cancelledTask, base.Add(8*time.Second), OutcomeBlocked, "", false)

	_, gateSpan, err := client.StartGate(runCtx, GateAttributes{
		StartedAt:    base.Add(9 * time.Second),
		Gaggle:       "acme-web",
		WorkflowID:   "compatibility-workflow",
		RunID:        "11111111111111111111111111111111",
		GateID:       "review",
		Decision:     "escalated",
		RepassNumber: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateSpan.SetGateResult("escalated", 2)
	completeSpanAt(gateSpan, base.Add(10*time.Second), "escalated", "", false)

	_, schedulerSpan, err := client.StartSchedulerSpan(context.Background(), SchedulerAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		Action:     "shutdown",
	})
	if err != nil {
		t.Fatal(err)
	}
	endSpanAt(schedulerSpan, base.Add(11*time.Second))

	completeSpanAt(runSpan, base.Add(5*time.Second), OutcomeSuccess, "", false)
	runSpan.End()

	client.InstanceJournalAppendDropped()
	client.SnapshotCaptured("delta", 4096)
	client.SnapshotCaptured("full", 1<<20)
	client.SnapshotFallback("no_base_ref")
	client.SnapshotRestoreFailed("base_missing")
	client.StorageHealthSampled("warning", 1024, true)
	client.RecordWorkcopyUsage(context.Background(), worktree.UsageMeasurement{
		Gaggle:           "acme-web",
		Operation:        worktree.UsageOperationCreate,
		OwnerRunID:       "11111111111111111111111111111111",
		WorktreeID:       "wt-c91a3be152bfaea1b",
		WorktreeMeasured: true,
		WorktreeBytes:    2048,
		WorkcopyMeasured: true,
		WorkcopyBytes:    8192,
	})
	client.scrubber.Scrub([]byte("ghp_0123456789abcdefghijklmnopqrstuvwxyzA"))

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := collector.waitForMetricRequests(t, 1)
	collector.stop()
	shutdownCompatibilityClient(t, client)

	return metricCompatibilityScenario{
		Name:        "all-metrics-temporality-and-terminal-paths",
		Description: "One force-flushed OTLP export exercising every metric instrument, repeated terminal calls, and active-work zeroing across success, failure, cancellation, and shutdown paths.",
		Requests:    marshalScenarioRequests(t, requests[:1]),
		Exports:     cloneMetricRequests(requests[:1]),
	}
}

func buildForceFlushThenPeriodicScenario(t *testing.T) metricCompatibilityScenario {
	t.Helper()

	collector := newMetricCompatibilityCollectorServer(t)
	client := newCompatibilityClient(t, collector.endpoint(), "daemon-flush-periodic", 75*time.Millisecond)

	base := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)
	runCtx, runSpan, err := client.StartRun(context.Background(), RunAttributes{
		StartedAt:  base,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "22222222222222222222222222222222",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		StartedAt:  base.Add(time.Second),
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "22222222222222222222222222222222",
		TaskID:     "periodic-task",
		TaskType:   StageTypeDeterministic,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(taskSpan, base.Add(2*time.Second), OutcomeSuccess, "", false)
	completeSpanAt(runSpan, base.Add(3*time.Second), OutcomeSuccess, "", false)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	requests := collector.waitForMetricRequests(t, 1)
	requests = collector.waitForMetricRequests(t, len(requests)+1)
	collector.stop()
	shutdownCompatibilityClient(t, client)

	return metricCompatibilityScenario{
		Name:        "force-flush-then-periodic",
		Description: "A force flush immediately exports pending cumulative metrics, and the later periodic collection re-emits the same stream without resetting or double-counting it.",
		Requests:    marshalScenarioRequests(t, requests[:2]),
		Exports:     cloneMetricRequests(requests[:2]),
	}
}

func buildDaemonRestartScenario(t *testing.T) metricCompatibilityScenario {
	t.Helper()

	collector := newMetricCompatibilityCollectorServer(t)
	first := newCompatibilityClient(t, collector.endpoint(), "daemon-before-restart", time.Hour)
	firstBase := time.Date(2026, 9, 16, 4, 0, 0, 0, time.UTC)
	_, firstRun, err := first.StartRun(context.Background(), RunAttributes{
		StartedAt:  firstBase,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "33333333333333333333333333333333",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(firstRun, firstBase.Add(5*time.Second), OutcomeSuccess, "", false)
	if err := first.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := collector.waitForMetricRequests(t, 1)
	collector.stop()
	shutdownCompatibilityClient(t, first)
	collector.reset()
	collector.start(t)

	second := newCompatibilityClient(t, collector.endpoint(), "daemon-after-restart", time.Hour)
	secondBase := time.Date(2026, 9, 16, 5, 0, 0, 0, time.UTC)
	_, secondRun, err := second.StartRun(context.Background(), RunAttributes{
		StartedAt:  secondBase,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "44444444444444444444444444444444",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(secondRun, secondBase.Add(7*time.Second), OutcomeSuccess, "", false)
	if err := second.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondRequests := collector.waitForMetricRequests(t, 1)
	collector.stop()
	shutdownCompatibilityClient(t, second)

	return metricCompatibilityScenario{
		Name:        "daemon-restart-new-cumulative-stream",
		Description: "Two daemon processes export the same cumulative instruments with stable resource identity except for a new service.instance.id, so cumulative-to-delta consumers can treat the restart as a new stream.",
		Requests:    marshalScenarioRequests(t, append(requests[:1], secondRequests[:1]...)),
		Exports:     cloneMetricRequests(append(requests[:1], secondRequests[:1]...)),
	}
}

func buildCollectorRestartScenario(t *testing.T) metricCompatibilityScenario {
	t.Helper()

	collector := newMetricCompatibilityCollectorServer(t)
	client := newCompatibilityClient(t, collector.endpoint(), "daemon-collector-restart", time.Hour)
	firstBase := time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC)
	_, runOne, err := client.StartRun(context.Background(), RunAttributes{
		StartedAt:  firstBase,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "55555555555555555555555555555555",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(runOne, firstBase.Add(time.Second), OutcomeSuccess, "", false)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	collector.waitForSnapshot(t, func(requests []*collectormetrics.ExportMetricsServiceRequest) bool {
		return requestWithCounterValue(requests, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 1) != nil
	})

	collector.stop()
	secondBase := time.Date(2026, 9, 16, 6, 1, 0, 0, time.UTC)
	_, runTwo, err := client.StartRun(context.Background(), RunAttributes{
		StartedAt:  secondBase,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "66666666666666666666666666666666",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(runTwo, secondBase.Add(time.Second), OutcomeSuccess, "", false)
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFlush()
	if err := client.Flush(flushCtx); err != nil {
		t.Fatalf("flush with collector down: %v", err)
	}

	collector.start(t)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	collector.waitForSnapshot(t, func(requests []*collectormetrics.ExportMetricsServiceRequest) bool {
		return requestWithCounterValue(requests, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 2) != nil
	})

	thirdBase := time.Date(2026, 9, 16, 6, 2, 0, 0, time.UTC)
	_, runThree, err := client.StartRun(context.Background(), RunAttributes{
		StartedAt:  thirdBase,
		Gaggle:     "acme-web",
		WorkflowID: "compatibility-workflow",
		RunID:      "77777777777777777777777777777777",
	})
	if err != nil {
		t.Fatal(err)
	}
	completeSpanAt(runThree, thirdBase.Add(time.Second), OutcomeSuccess, "", false)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := collector.waitForSnapshot(t, func(requests []*collectormetrics.ExportMetricsServiceRequest) bool {
		return requestWithCounterValue(requests, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 3) != nil
	})
	collector.stop()
	shutdownCompatibilityClient(t, client)

	selected := []*collectormetrics.ExportMetricsServiceRequest{
		requestWithCounterValue(recovered, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 1),
		requestWithCounterValue(recovered, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 2),
		requestWithCounterValue(recovered, MetricRunOutcomes, map[string]string{
			AttrWorkflow: "compatibility-workflow",
			AttrOutcome:  OutcomeSuccess,
		}, 3),
	}
	for i, req := range selected {
		if req == nil {
			t.Fatalf("collector scenario missing selected request %d", i+1)
		}
	}

	return metricCompatibilityScenario{
		Name:        "collector-restart-recovery",
		Description: "The daemon keeps working while the OTLP collector is down, the first export after collector recovery succeeds again, and later work keeps extending the same cumulative stream without changing the existing best-effort availability contract.",
		Requests:    marshalScenarioRequests(t, selected),
		Exports:     cloneMetricRequests(selected),
	}
}

func newCompatibilityClient(t *testing.T, endpoint, instanceID string, interval time.Duration) *Client {
	t.Helper()
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id="+instanceID)
	client, err := New(context.Background(), Config{
		ServiceName:          "telemetry-compatibility",
		ServiceVersion:       "v1.0.0",
		BuildCommit:          "compatibility-fixture",
		Environment:          "test",
		SpanExporter:         telemetrytest.NewMemoryExporter(),
		Exporter:             ExporterOTLP,
		OTLPEndpoint:         "http://" + endpoint,
		OTLPInsecure:         true,
		MetricExportInterval: interval,
		Batch:                true,
		ResourceAttributes: []attribute.KeyValue{
			attribute.String("goobers.fixture", "metric-compatibility"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func shutdownCompatibilityClient(t *testing.T, client *Client) {
	t.Helper()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

type metricCompatibilityCollectorServer struct {
	t         *testing.T
	addr      string
	collector *recordingCompatibilityMetricCollector
	server    *grpc.Server
	listener  net.Listener
}

func newMetricCompatibilityCollectorServer(t *testing.T) *metricCompatibilityCollectorServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &metricCompatibilityCollectorServer{
		t:         t,
		addr:      listener.Addr().String(),
		collector: &recordingCompatibilityMetricCollector{notify: make(chan struct{}, 32)},
		server:    grpc.NewServer(),
		listener:  listener,
	}
	collectormetrics.RegisterMetricsServiceServer(server.server, server.collector)
	collectortrace.RegisterTraceServiceServer(server.server, traceNoopCollector{})
	go func() {
		_ = server.server.Serve(listener)
	}()
	return server
}

func (s *metricCompatibilityCollectorServer) endpoint() string {
	return s.addr
}

func (s *metricCompatibilityCollectorServer) start(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	s.server = grpc.NewServer()
	collectormetrics.RegisterMetricsServiceServer(s.server, s.collector)
	collectortrace.RegisterTraceServiceServer(s.server, traceNoopCollector{})
	s.listener = listener
	go func() {
		_ = s.server.Serve(listener)
	}()
}

func (s *metricCompatibilityCollectorServer) stop() {
	if s.server != nil {
		s.server.Stop()
		s.server = nil
	}
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
}

func (s *metricCompatibilityCollectorServer) reset() {
	s.collector.reset()
}

func (s *metricCompatibilityCollectorServer) waitForMetricRequests(t *testing.T, want int) []*collectormetrics.ExportMetricsServiceRequest {
	t.Helper()
	return s.collector.waitForRequests(t, want)
}

func (s *metricCompatibilityCollectorServer) waitForSnapshot(
	t *testing.T,
	predicate func([]*collectormetrics.ExportMetricsServiceRequest) bool,
) []*collectormetrics.ExportMetricsServiceRequest {
	t.Helper()
	return s.collector.waitForSnapshot(t, predicate)
}

type traceNoopCollector struct {
	collectortrace.UnimplementedTraceServiceServer
}

func (traceNoopCollector) Export(context.Context, *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type recordingCompatibilityMetricCollector struct {
	collectormetrics.UnimplementedMetricsServiceServer
	mu       sync.Mutex
	requests []*collectormetrics.ExportMetricsServiceRequest
	notify   chan struct{}
}

func (c *recordingCompatibilityMetricCollector) Export(
	_ context.Context,
	req *collectormetrics.ExportMetricsServiceRequest,
) (*collectormetrics.ExportMetricsServiceResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, proto.Clone(req).(*collectormetrics.ExportMetricsServiceRequest))
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

func (c *recordingCompatibilityMetricCollector) waitForRequests(t *testing.T, want int) []*collectormetrics.ExportMetricsServiceRequest {
	return c.waitForSnapshot(t, func(snapshot []*collectormetrics.ExportMetricsServiceRequest) bool {
		return len(snapshot) >= want
	})
}

func (c *recordingCompatibilityMetricCollector) waitForSnapshot(
	t *testing.T,
	predicate func([]*collectormetrics.ExportMetricsServiceRequest) bool,
) []*collectormetrics.ExportMetricsServiceRequest {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		snapshot := c.snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		select {
		case <-c.notify:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("collector never satisfied compatibility predicate; last snapshot had %d metric requests", len(snapshot))
		}
	}
}

func (c *recordingCompatibilityMetricCollector) snapshot() []*collectormetrics.ExportMetricsServiceRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*collectormetrics.ExportMetricsServiceRequest(nil), c.requests...)
}

func (c *recordingCompatibilityMetricCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = nil
	for {
		select {
		case <-c.notify:
		default:
			return
		}
	}
}

func marshalScenarioRequests(t *testing.T, requests []*collectormetrics.ExportMetricsServiceRequest) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(requests))
	for _, req := range requests {
		normalized := normalizeMetricExportRequest(proto.Clone(req).(*collectormetrics.ExportMetricsServiceRequest))
		data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(normalized)
		if err != nil {
			t.Fatalf("marshal OTLP metric request: %v", err)
		}
		out = append(out, data)
	}
	return out
}

func cloneMetricRequests(requests []*collectormetrics.ExportMetricsServiceRequest) []*collectormetrics.ExportMetricsServiceRequest {
	out := make([]*collectormetrics.ExportMetricsServiceRequest, 0, len(requests))
	for _, req := range requests {
		out = append(out, proto.Clone(req).(*collectormetrics.ExportMetricsServiceRequest))
	}
	return out
}

func normalizeMetricExportRequest(req *collectormetrics.ExportMetricsServiceRequest) *collectormetrics.ExportMetricsServiceRequest {
	for _, resourceMetrics := range req.ResourceMetrics {
		sortKeyValues(resourceMetrics.Resource.Attributes)
		slices.SortFunc(resourceMetrics.ScopeMetrics, func(a, b *metricspb.ScopeMetrics) int {
			return strings.Compare(a.Scope.Name, b.Scope.Name)
		})
		for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
			sortKeyValues(scopeMetrics.Scope.Attributes)
			slices.SortFunc(scopeMetrics.Metrics, func(a, b *metricspb.Metric) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, metric := range scopeMetrics.Metrics {
				normalizeMetric(metric)
			}
		}
	}
	return req
}

func normalizeMetric(metric *metricspb.Metric) {
	switch data := metric.Data.(type) {
	case *metricspb.Metric_Sum:
		normalizeNumberPoints(data.Sum.DataPoints)
	case *metricspb.Metric_Gauge:
		normalizeNumberPoints(data.Gauge.DataPoints)
	case *metricspb.Metric_Histogram:
		normalizeHistogramPoints(metric.Name, data.Histogram.DataPoints)
	}
}

func normalizeNumberPoints(points []*metricspb.NumberDataPoint) {
	for _, point := range points {
		sortKeyValues(point.Attributes)
		point.StartTimeUnixNano = 0
		point.TimeUnixNano = 0
		for _, exemplar := range point.Exemplars {
			sortKeyValues(exemplar.FilteredAttributes)
			exemplar.TimeUnixNano = 0
		}
	}
	slices.SortFunc(points, func(a, b *metricspb.NumberDataPoint) int {
		return strings.Compare(attributeSignature(a.Attributes), attributeSignature(b.Attributes))
	})
}

func normalizeHistogramPoints(metricName string, points []*metricspb.HistogramDataPoint) {
	for _, point := range points {
		sortKeyValues(point.Attributes)
		point.StartTimeUnixNano = 0
		point.TimeUnixNano = 0
		// RecordWorkcopyUsage creates and completes this standalone scheduler
		// span around the measurement call, so its duration is deliberately
		// wall-clock based. Preserve the point's schema and count in the wire
		// fixture without making the golden file depend on runner timing.
		if metricName == MetricStageDuration && attributesContain(point.Attributes, map[string]string{
			AttrStage: "workcopy-create",
		}) {
			point.Sum = proto.Float64(0)
			point.Min = proto.Float64(0)
			point.Max = proto.Float64(0)
			for i := range point.BucketCounts {
				point.BucketCounts[i] = 0
			}
			if len(point.BucketCounts) > 0 {
				point.BucketCounts[0] = point.Count
			}
		}
		for _, exemplar := range point.Exemplars {
			sortKeyValues(exemplar.FilteredAttributes)
			exemplar.TimeUnixNano = 0
		}
	}
	slices.SortFunc(points, func(a, b *metricspb.HistogramDataPoint) int {
		return strings.Compare(attributeSignature(a.Attributes), attributeSignature(b.Attributes))
	})
}

func sortKeyValues(attrs []*commonpb.KeyValue) {
	slices.SortFunc(attrs, func(a, b *commonpb.KeyValue) int {
		return strings.Compare(a.Key, b.Key)
	})
}

func attributeSignature(attrs []*commonpb.KeyValue) string {
	var b strings.Builder
	for _, attr := range attrs {
		b.WriteString(attr.Key)
		b.WriteByte('=')
		b.WriteString(anyValueSignature(attr.Value))
		b.WriteByte(';')
	}
	return b.String()
}

func anyValueSignature(value *commonpb.AnyValue) string {
	if value == nil {
		return "<nil>"
	}
	switch v := value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_BoolValue:
		if v.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", v.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", v.DoubleValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			parts = append(parts, anyValueSignature(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return fmt.Sprintf("%T", value.Value)
	}
}

func metricByName(t *testing.T, req *collectormetrics.ExportMetricsServiceRequest, name string) *metricspb.Metric {
	t.Helper()
	for _, resourceMetrics := range req.GetResourceMetrics() {
		for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
			for _, metric := range scopeMetrics.GetMetrics() {
				if metric.GetName() == name {
					return metric
				}
			}
		}
	}
	t.Fatalf("metric %s missing from request", name)
	return nil
}

func assertMetricKindAndTemporality(
	t *testing.T,
	req *collectormetrics.ExportMetricsServiceRequest,
	name, kind string,
	temporality metricspb.AggregationTemporality,
	monotonic bool,
) {
	t.Helper()
	metric := metricByName(t, req, name)
	switch data := metric.Data.(type) {
	case *metricspb.Metric_Sum:
		if kind != "sum" {
			t.Fatalf("metric %s kind = sum, want %s", name, kind)
		}
		if data.Sum.AggregationTemporality != temporality || data.Sum.IsMonotonic != monotonic {
			t.Fatalf("metric %s temporality/monotonic = %s/%t, want %s/%t",
				name, data.Sum.AggregationTemporality, data.Sum.IsMonotonic, temporality, monotonic)
		}
	case *metricspb.Metric_Histogram:
		if kind != "histogram" {
			t.Fatalf("metric %s kind = histogram, want %s", name, kind)
		}
		if data.Histogram.AggregationTemporality != temporality {
			t.Fatalf("metric %s temporality = %s, want %s", name, data.Histogram.AggregationTemporality, temporality)
		}
	default:
		t.Fatalf("metric %s type = %T, want %s", name, metric.Data, kind)
	}
}

func assertGaugeMetric(t *testing.T, req *collectormetrics.ExportMetricsServiceRequest, name string) {
	t.Helper()
	metric := metricByName(t, req, name)
	if _, ok := metric.Data.(*metricspb.Metric_Gauge); !ok {
		t.Fatalf("metric %s type = %T, want gauge", name, metric.Data)
	}
}

type numberPointValue struct {
	AsInt int64
}

func findNumberPoint(t *testing.T, metric *metricspb.Metric, subset map[string]string) numberPointValue {
	t.Helper()
	switch data := metric.Data.(type) {
	case *metricspb.Metric_Sum:
		for _, point := range data.Sum.DataPoints {
			if attributesContain(point.Attributes, subset) {
				return numberDataPointValue(point)
			}
		}
	case *metricspb.Metric_Gauge:
		for _, point := range data.Gauge.DataPoints {
			if attributesContain(point.Attributes, subset) {
				return numberDataPointValue(point)
			}
		}
	default:
		t.Fatalf("metric %s type = %T, want sum or gauge", metric.Name, metric.Data)
	}
	t.Fatalf("metric %s missing point with attrs %v", metric.Name, subset)
	return numberPointValue{}
}

func numberDataPointValue(point *metricspb.NumberDataPoint) numberPointValue {
	if point == nil {
		return numberPointValue{}
	}
	if value := point.GetAsInt(); value != 0 || point.Value != nil {
		return numberPointValue{AsInt: value}
	}
	return numberPointValue{}
}

func findHistogramPoint(t *testing.T, metric *metricspb.Metric, subset map[string]string) *metricspb.HistogramDataPoint {
	t.Helper()
	data, ok := metric.Data.(*metricspb.Metric_Histogram)
	if !ok {
		t.Fatalf("metric %s type = %T, want histogram", metric.Name, metric.Data)
	}
	for _, point := range data.Histogram.DataPoints {
		if attributesContain(point.Attributes, subset) {
			return point
		}
	}
	t.Fatalf("metric %s missing histogram point with attrs %v", metric.Name, subset)
	return nil
}

func attributesContain(attrs []*commonpb.KeyValue, subset map[string]string) bool {
	got := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		got[attr.Key] = anyValueSignature(attr.Value)
	}
	for key, want := range subset {
		if got[key] != want {
			return false
		}
	}
	return true
}

func resourceAttributeMap(req *collectormetrics.ExportMetricsServiceRequest) map[string]string {
	out := map[string]string{}
	for _, resourceMetrics := range req.GetResourceMetrics() {
		for _, attr := range resourceMetrics.GetResource().GetAttributes() {
			out[attr.Key] = anyValueSignature(attr.Value)
		}
	}
	return out
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func requestWithCounterValue(
	requests []*collectormetrics.ExportMetricsServiceRequest,
	name string,
	subset map[string]string,
	want int64,
) *collectormetrics.ExportMetricsServiceRequest {
	for _, req := range requests {
		if got, ok := counterValue(req, name, subset); ok && got == want {
			return req
		}
	}
	return nil
}

func counterValue(
	req *collectormetrics.ExportMetricsServiceRequest,
	name string,
	subset map[string]string,
) (int64, bool) {
	metric := findMetric(req, name)
	if metric == nil {
		return 0, false
	}
	data, ok := metric.Data.(*metricspb.Metric_Sum)
	if !ok {
		return 0, false
	}
	for _, point := range data.Sum.DataPoints {
		if attributesContain(point.Attributes, subset) {
			return point.GetAsInt(), true
		}
	}
	return 0, false
}

func findMetric(req *collectormetrics.ExportMetricsServiceRequest, name string) *metricspb.Metric {
	for _, resourceMetrics := range req.GetResourceMetrics() {
		for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
			for _, metric := range scopeMetrics.GetMetrics() {
				if metric.GetName() == name {
					return metric
				}
			}
		}
	}
	return nil
}

type cumulativeDeltaGateway struct {
	counters   map[string]int64
	histograms map[string]histogramTotal
}

type histogramTotal struct {
	count uint64
	sum   float64
}

func newCumulativeDeltaGateway() *cumulativeDeltaGateway {
	return &cumulativeDeltaGateway{
		counters:   map[string]int64{},
		histograms: map[string]histogramTotal{},
	}
}

func (g *cumulativeDeltaGateway) observeCounter(
	t *testing.T,
	req *collectormetrics.ExportMetricsServiceRequest,
	name string,
	subset map[string]string,
) int64 {
	t.Helper()
	point := findNumberPoint(t, metricByName(t, req, name), subset)
	key := streamKey(req, name, subset)
	previous, ok := g.counters[key]
	g.counters[key] = point.AsInt
	if !ok || point.AsInt < previous {
		return point.AsInt
	}
	return point.AsInt - previous
}

func (g *cumulativeDeltaGateway) observeHistogram(
	t *testing.T,
	req *collectormetrics.ExportMetricsServiceRequest,
	name string,
	subset map[string]string,
) (uint64, float64) {
	t.Helper()
	point := findHistogramPoint(t, metricByName(t, req, name), subset)
	key := streamKey(req, name, subset)
	previous, ok := g.histograms[key]
	current := histogramTotal{count: point.Count}
	if point.Sum != nil {
		current.sum = *point.Sum
	}
	g.histograms[key] = current
	if !ok || current.count < previous.count || current.sum < previous.sum {
		return current.count, current.sum
	}
	return current.count - previous.count, current.sum - previous.sum
}

func streamKey(req *collectormetrics.ExportMetricsServiceRequest, name string, subset map[string]string) string {
	resource := resourceAttributeMap(req)
	parts := []string{
		"service.name=" + resource["service.name"],
		"service.instance.id=" + resource["service.instance.id"],
		"metric=" + name,
	}
	keys := make([]string, 0, len(subset))
	for key := range subset {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		parts = append(parts, key+"="+subset[key])
	}
	return strings.Join(parts, "|")
}

func endSpanAt(span Span, endedAt time.Time) {
	if span.span == nil {
		return
	}
	span.span.End(trace.WithTimestamp(endedAt))
	span.metrics.record(endedAt, outcomeUnset, "")
}

func completeSpanAt(span Span, endedAt time.Time, outcome, errorCode string, isFailure bool) {
	if span.span == nil {
		return
	}
	outcome = span.scrub(outcome)
	attrs := []attribute.KeyValue{attribute.String(AttrOutcome, outcome)}
	if errorCode != "" {
		errorCode = span.scrub(errorCode)
		attrs = append(attrs, attribute.String(AttrErrorCode, errorCode))
	}
	if isFailure {
		errorType := errorCode
		if errorType == "" {
			errorType = outcome
		}
		attrs = append(attrs, attribute.String(AttrErrorType, errorType))
	}
	span.span.SetAttributes(attrs...)
	if isFailure {
		span.span.SetStatus(codes.Error, outcome)
	} else {
		span.span.SetStatus(codes.Ok, outcome)
	}
	span.span.End(trace.WithTimestamp(endedAt))
	span.metrics.record(endedAt, outcome, errorCode)
}

func failSpanAt(span Span, endedAt time.Time, err error, errorCode string) {
	if span.span == nil {
		return
	}
	if err == nil {
		err = errors.New("span failed")
	}
	message := span.scrub(err.Error())
	errorType := fmt.Sprintf("%T", err)
	if errorCode != "" {
		errorType = errorCode
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrOutcome, OutcomeFailure),
		attribute.String(AttrErrorType, span.scrub(errorType)),
	}
	if errorCode != "" {
		errorCode = span.scrub(errorCode)
		attrs = append(attrs, attribute.String(AttrErrorCode, errorCode))
	}
	span.span.SetAttributes(attrs...)
	span.span.RecordError(errors.New(message))
	span.span.SetStatus(codes.Error, message)
	span.span.End(trace.WithTimestamp(endedAt))
	span.metrics.record(endedAt, OutcomeFailure, errorCode)
}
