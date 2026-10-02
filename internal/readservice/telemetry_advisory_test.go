package readservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

type storedAdvisoryDecider struct {
	calls       atomic.Int32
	confidence  float64
	probability float64
}

func (d *storedAdvisoryDecider) Decide(_ context.Context, request decider.Request) (decider.Response, error) {
	d.calls.Add(1)
	answers := make(map[string]decider.Answer, len(request.Questions))
	for name := range request.Questions {
		confidence := d.confidence
		if confidence == 0 {
			confidence = 0.9
		}
		probability := d.probability
		if probability == 0 {
			probability = 0.9
		}
		answers[name] = decider.Answer{
			Type: decider.KindChoice, Choice: string(creditgraph.ClassEnvironment),
			Confidence: &confidence,
			Probabilities: map[string]float64{
				string(creditgraph.ClassEnvironment): probability,
				string(creditgraph.ClassUnknown):     1 - probability,
			},
		}
	}
	return decider.Response{Model: "failure-classifier-v1", Answers: answers}, nil
}

func TestStoredModelAssistedShadowReplaysAndComparesLaterRuleOutcome(t *testing.T) {
	root := t.TempDir()
	runID := "advisory-run"
	writeAuditRecord(t, root, runID, "v1", false)
	writeAdvisoryCause(t, root, runID, creditgraph.ClassUnknown)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow(runID, now)},
	}}}
	model := &storedAdvisoryDecider{}
	gate, err := decisiongate.New(model, decisiongate.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	classifier := creditgraph.AdvisoryClassifier{Gate: gate, Model: "failure-classifier-v1"}

	for range 2 {
		if err := StoredModelAssistedShadow(
			context.Background(), root, reader, StoredAttributionQuery{}, classifier,
		); err != nil {
			t.Fatal(err)
		}
	}
	if model.calls.Load() != 1 {
		t.Fatalf("model calls = %d, want persisted digest replay", model.calls.Load())
	}
	records, err := ReadModelAssistedShadow(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Finding == nil ||
		records[0].Finding.Class != creditgraph.ClassEnvironment ||
		records[0].MatchesOutcome != nil {
		t.Fatalf("records = %+v, want one unresolved advisory finding", records)
	}

	writeAdvisoryCause(t, root, runID, creditgraph.ClassEnvironment)
	if err := StoredModelAssistedShadow(
		context.Background(), root, reader, StoredAttributionQuery{}, classifier,
	); err != nil {
		t.Fatal(err)
	}
	records, err = ReadModelAssistedShadow(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].OutcomeClass != creditgraph.ClassEnvironment ||
		records[0].OutcomeSource != "deterministic-rule" ||
		records[0].MatchesOutcome == nil || !*records[0].MatchesOutcome {
		t.Fatalf("records = %+v, want matching later rule outcome", records)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("model calls after outcome = %d, want no re-scoring", model.calls.Load())
	}
}

func TestStoredModelAssistedShadowComparesThresholdedSuggestion(t *testing.T) {
	root := t.TempDir()
	runID := "thresholded-advisory-run"
	writeAuditRecord(t, root, runID, "v1", false)
	writeAdvisoryCause(t, root, runID, creditgraph.ClassUnknown)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow(runID, time.Now())},
	}}}
	model := &storedAdvisoryDecider{confidence: 0.7, probability: 0.9}
	gate, err := decisiongate.New(model, decisiongate.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	classifier := creditgraph.AdvisoryClassifier{Gate: gate, Model: "failure-classifier-v1"}
	if err := StoredModelAssistedShadow(
		context.Background(), root, reader, StoredAttributionQuery{}, classifier,
	); err != nil {
		t.Fatal(err)
	}

	writeAdvisoryCause(t, root, runID, creditgraph.ClassEnvironment)
	if err := StoredModelAssistedShadow(
		context.Background(), root, reader, StoredAttributionQuery{}, classifier,
	); err != nil {
		t.Fatal(err)
	}
	records, err := ReadModelAssistedShadow(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Finding == nil ||
		records[0].Finding.Class != creditgraph.ClassUnknown ||
		records[0].Finding.SuggestedClass != creditgraph.ClassEnvironment ||
		records[0].MatchesOutcome == nil || !*records[0].MatchesOutcome {
		t.Fatalf("records = %+v, want matching thresholded suggestion", records)
	}
}

func TestRecordModelAssistedHumanOutcomePreservesProvenance(t *testing.T) {
	root := t.TempDir()
	runID := "human-advisory-run"
	writeAuditRecord(t, root, runID, "v1", false)
	writeAdvisoryCause(t, root, runID, creditgraph.ClassUnknown)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow(runID, time.Now())},
	}}}
	model := &storedAdvisoryDecider{}
	gate, err := decisiongate.New(model, decisiongate.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	classifier := creditgraph.AdvisoryClassifier{Gate: gate, Model: "failure-classifier-v1"}
	if err := StoredModelAssistedShadow(
		context.Background(), root, reader, StoredAttributionQuery{}, classifier,
	); err != nil {
		t.Fatal(err)
	}
	if err := RecordModelAssistedHumanOutcome(
		context.Background(), root, runID, "stage:implement#1",
		creditgraph.ClassEnvironment, "review:backprop-42",
	); err != nil {
		t.Fatal(err)
	}

	records, err := ReadModelAssistedShadow(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].OutcomeClass != creditgraph.ClassEnvironment ||
		records[0].OutcomeSource != "human" ||
		records[0].OutcomeProvenance != "review:backprop-42" ||
		records[0].MatchesOutcome == nil || !*records[0].MatchesOutcome {
		t.Fatalf("records = %+v, want matching human outcome with provenance", records)
	}

	writeAdvisoryCause(t, root, runID, creditgraph.ClassModel)
	if err := StoredModelAssistedShadow(
		context.Background(), root, reader, StoredAttributionQuery{}, classifier,
	); err != nil {
		t.Fatal(err)
	}
	records, err = ReadModelAssistedShadow(root)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].OutcomeSource != "human" ||
		records[0].OutcomeProvenance != "review:backprop-42" ||
		records[0].OutcomeClass != creditgraph.ClassEnvironment {
		t.Fatalf("records = %+v, want human outcome retained", records)
	}
}

func TestTelemetryStatsIgnoresInvalidModelAssistedShadowState(t *testing.T) {
	root, store, _ := seedStoredAttributionRun(t)
	path := modelAssistedShadowPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := &storedAdvisoryDecider{}
	gate, err := decisiongate.New(model, decisiongate.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := &Local{
		sources: LocalSources{
			Layout:    instance.NewLayout(root),
			ReadModel: store,
			CreditAdvisory: &creditgraph.AdvisoryClassifier{
				Gate: gate, Model: "failure-classifier-v1",
			},
		},
		telemetry: &Telemetry{store: &fakeTelemetryStore{}},
	}

	result, err := service.TelemetryStats(context.Background(), TelemetryStatsRequest{})
	if err != nil {
		t.Fatalf("telemetry stats: %v", err)
	}
	if len(result.AttributionCohorts) != 1 {
		t.Fatalf("attribution cohorts = %+v, want normal telemetry result", result.AttributionCohorts)
	}
	if model.calls.Load() != 0 {
		t.Fatalf("model calls = %d, want invalid state rejected before scoring", model.calls.Load())
	}
}

func writeAdvisoryCause(t *testing.T, root, runID string, class creditgraph.FailureClass) {
	t.Helper()
	path := filepath.Join(instance.NewLayout(root).RunsDir(), runID, creditgraph.RecordFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record creditgraph.RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Attribution = creditgraph.Attribution{
		Schema: creditgraph.AttributionSchemaVersion,
		RunID:  runID, RootID: "outcome", Outcome: "failed",
		Contributions: []creditgraph.Contribution{{
			NodeID: "stage:implement#1", Stage: "implement", Share: 1, Score: -1,
			Uncertainty: 0.4, Confidence: 0.6,
		}},
		Causes: []creditgraph.CauseFinding{{
			Class: class, NodeID: "stage:implement#1", Stage: "implement",
			Confidence: 0.6, Summary: "recorded test cause",
		}},
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
