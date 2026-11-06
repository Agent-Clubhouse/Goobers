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
)

func publishedReviewerEval() creditgraph.ReviewerEvalSummary {
	return creditgraph.ReviewerEvalSummary{
		Schema: creditgraph.ReviewerEvalSummarySchemaVersion, DatasetVersion: "v1", DatasetDigest: "sha256:abc",
		Split: creditgraph.ReviewerEvalSplit, Variant: "baseline", OutOfDiff: 1,
		Overall: creditgraph.ReviewerEvalMetrics{Caught: 1, Evaluated: 2, Recall: 0.5, MatchedFindings: 1, Findings: 4, Precision: 0.25},
		Classes: []creditgraph.ReviewerEvalMetrics{
			{Class: "missing-tests", Evaluated: 2, Findings: 1},
			{Class: "substantive", Caught: 1, Evaluated: 2, Recall: 0.5, MatchedFindings: 1, Findings: 3, Precision: 0.3333},
		},
		TotalCostUSD: 3, CostPerBugUSD: 1.5,
	}
}

func TestLocalTelemetryStatsReportsPublishedReviewerEval(t *testing.T) {
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
	if err != nil || unpublished.ReviewerEval != nil {
		t.Fatalf("unpublished stats reviewer eval = %+v, %v; want none", unpublished.ReviewerEval, err)
	}

	summary := publishedReviewerEval()
	if err := PublishReviewerEval(root, summary); err != nil {
		t.Fatal(err)
	}
	published, err := service.TelemetryStats(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := published.ReviewerEval; got == nil || got.DatasetDigest != summary.DatasetDigest || got.Overall != summary.Overall {
		t.Fatalf("stats reviewer eval = %+v, want the published summary", got)
	}
}

func TestReviewerEvalRejectsTrainingSplitAndMalformedSummary(t *testing.T) {
	root := t.TempDir()
	training := publishedReviewerEval()
	training.Split = "train"
	if err := PublishReviewerEval(root, training); err == nil || !strings.Contains(err.Error(), "only the \"eval\" split") {
		t.Fatalf("PublishReviewerEval(train) = %v, want the training split refused", err)
	}
	if err := os.MkdirAll(filepath.Dir(reviewerEvalPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := `{"schema":"` + creditgraph.ReviewerEvalSummarySchemaVersion + `","datasetVersion":"v1","datasetDigest":"d","split":"eval","variant":"b","overall":{"caught":2,"evaluated":1}}`
	if err := os.WriteFile(reviewerEvalPath(root), []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if summary, err := ReadReviewerEval(root); err == nil || summary != nil {
		t.Fatalf("ReadReviewerEval() = %+v, %v; want the malformed summary reported", summary, err)
	}
}
