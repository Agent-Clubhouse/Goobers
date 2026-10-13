package readservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

// imprecisePublishedEval is a calibration whose only populated bin (0.8-1)
// measured 1/5 correct, so the derived filing threshold is 1.
func imprecisePublishedEval() creditgraph.AttributionEvalSummary {
	return creditgraph.AttributionEvalSummary{
		Schema: creditgraph.AttributionEvalSummarySchemaVersion, EvalSetVersion: "v1", EvalSetDigest: "sha256:abc",
		CaseCount: 5,
		Accuracy:  []creditgraph.AttributionEvalAccuracy{{Granularity: "domain", Correct: 1, Total: 5, Accuracy: 0.2}},
		Calibration: creditgraph.AttributionEvalCalibration{Target: "domain", Bins: []creditgraph.AttributionEvalBin{
			{Lower: 0, Upper: 0.8},
			{Lower: 0.8, Upper: 1, Count: 5, Correct: 1, MeanConfidence: 0.9, Accuracy: 0.2},
		}},
	}
}

func TestPublishedAttributionEvalSetsFilingThresholdAndCooldown(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordWithEnvironment(t, root, "before", "v1", true, "windows")
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("before", now.Add(-time.Hour))},
	}}}
	config := creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1}

	if summary, err := ReadAttributionEval(root); err != nil || summary != nil {
		t.Fatalf("ReadAttributionEval() = %+v, %v; want nothing published", summary, err)
	}
	if err := PublishAttributionEval(root, imprecisePublishedEval()); err != nil {
		t.Fatal(err)
	}
	published, err := ReadAttributionEval(root)
	if err != nil || published == nil || published.EvalSetDigest != "sha256:abc" {
		t.Fatalf("ReadAttributionEval() = %+v, %v; want the published summary", published, err)
	}

	for pass := range 2 {
		config.Now = now.Add(time.Duration(pass) * time.Hour)
		report, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
		if err != nil {
			t.Fatal(err)
		}
		want := creditgraph.FilingThreshold{
			Confidence: 1, Source: creditgraph.FilingThresholdAttributionEval, CalibrationTarget: "domain",
			EvalSetVersion: "v1", EvalSetDigest: "sha256:abc",
		}
		if report.FilingThreshold == nil || *report.FilingThreshold != want {
			t.Fatalf("pass %d filing threshold = %+v, want %+v", pass, report.FilingThreshold, want)
		}
		if report.Suppressed != 0 || len(report.WorkflowFindings) != 1 || report.WorkflowFindings[0].Fileable {
			t.Fatalf("pass %d report = %+v, want one unfileable finding that never starts the cooldown", pass, report)
		}
	}
}

func TestReadAttributionEvalRejectsMalformedSummary(t *testing.T) {
	root := t.TempDir()
	invalid := imprecisePublishedEval()
	invalid.CaseCount = 6
	if err := PublishAttributionEval(root, invalid); err == nil {
		t.Fatal("PublishAttributionEval() = nil, want a validation error")
	}
	path := attributionEvalPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":"goobers.dev/backprop/attribution-eval-summary/v0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{}}}
	_, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, creditgraph.FaultAuditConfig{})
	if err == nil || !strings.Contains(err.Error(), "decode attribution eval") {
		t.Fatalf("StoredFaultAudit() error = %v, want the malformed eval reported, not the default threshold", err)
	}
}

func TestLocalTelemetryStatsPublishesAttributionEval(t *testing.T) {
	root, store, _ := seedStoredAttributionRun(t)
	service := &Local{
		sources:   LocalSources{Layout: instance.NewLayout(root), ReadModel: store},
		telemetry: &Telemetry{store: &telemetryStoreWithoutInvocationReader{telemetryStore: &fakeTelemetryStore{}}},
	}
	request := TelemetryStatsRequest{
		Gaggle: "core", Workflow: "implementation", Since: time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC),
	}

	unpublished, err := service.TelemetryStats(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if unpublished.AttributionEval != nil || unpublished.FaultAudit == nil ||
		*unpublished.FaultAudit.FilingThreshold != creditgraph.DeriveFilingThreshold(nil) {
		t.Fatalf("unpublished stats eval = %+v audit = %+v, want no eval and the default threshold",
			unpublished.AttributionEval, unpublished.FaultAudit)
	}

	summary := imprecisePublishedEval()
	if err := PublishAttributionEval(root, summary); err != nil {
		t.Fatal(err)
	}
	published, err := service.TelemetryStats(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if published.AttributionEval == nil || published.AttributionEval.EvalSetDigest != summary.EvalSetDigest {
		t.Fatalf("stats attribution eval = %+v, want the published summary", published.AttributionEval)
	}
	if got := published.FaultAudit.FilingThreshold; got == nil || *got != creditgraph.DeriveFilingThreshold(&summary) {
		t.Fatalf("stats filing threshold = %+v, want one derived from the published eval", got)
	}
	supplied, err := service.TelemetryAttribution(context.Background(), TelemetryAttributionRequest{
		Observations: []creditgraph.AttributionObservation{{RunID: "supplied"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := supplied.Audit.FilingThreshold; got == nil || *got != creditgraph.DeriveFilingThreshold(&summary) {
		t.Fatalf("supplied-observation filing threshold = %+v, want one derived from the published eval", got)
	}
}
