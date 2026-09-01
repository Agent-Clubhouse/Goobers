package httpapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

var fixedTelemetryTime = time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)

// TestImplementationOutcomeWireShapesAgree pins the three restatements of one
// shape against each other: the rollup row the daemon reads, the read
// service's projection that goes on the wire, and the stage-side client's
// restatement of that wire shape (internal/telemetryclient restates rather
// than imports, exactly as internal/claimsclient does — so this is the test
// that keeps the restatement honest).
//
// Compared by MARSHALLED JSON rather than by field list, because the wire is
// what actually has to agree: a renamed tag, a dropped omitempty, or a
// reordered field would all show up here.
func TestImplementationOutcomeWireShapesAgree(t *testing.T) {
	row := rollup.ImplementationOutcome{
		RunID:        "run-3",
		ItemID:       "912",
		Status:       "failed",
		StartedAt:    fixedTelemetryTime,
		FinishedAt:   fixedTelemetryTime,
		Stage:        "implement",
		ErrorCode:    "harness.crash",
		ErrorMessage: "boom",
		Gate:         "review",
		Verdict:      "reject",
	}
	projected := readservice.TelemetryImplementationOutcome{
		RunID:        row.RunID,
		ItemID:       row.ItemID,
		Status:       row.Status,
		StartedAt:    row.StartedAt,
		FinishedAt:   row.FinishedAt,
		Stage:        row.Stage,
		ErrorCode:    row.ErrorCode,
		ErrorMessage: row.ErrorMessage,
		Gate:         row.Gate,
		Verdict:      row.Verdict,
	}
	restated := telemetryclient.ImplementationOutcome{
		RunID:        row.RunID,
		ItemID:       row.ItemID,
		Status:       row.Status,
		StartedAt:    row.StartedAt,
		FinishedAt:   row.FinishedAt,
		Stage:        row.Stage,
		ErrorCode:    row.ErrorCode,
		ErrorMessage: row.ErrorMessage,
		Gate:         row.Gate,
		Verdict:      row.Verdict,
	}

	rowJSON := mustMarshal(t, row)
	projectedJSON := mustMarshal(t, projected)
	restatedJSON := mustMarshal(t, restated)
	if projectedJSON != rowJSON {
		t.Fatalf("read-service projection = %s, rollup row = %s", projectedJSON, rowJSON)
	}
	if restatedJSON != projectedJSON {
		t.Fatalf("client restatement = %s, read-service projection = %s", restatedJSON, projectedJSON)
	}

	// A client decode of a server encode must round-trip every field, so the
	// stage sees the evidence the daemon sent rather than a lossy subset.
	var decoded telemetryclient.ImplementationOutcome
	if err := json.Unmarshal([]byte(projectedJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, restated) {
		t.Fatalf("decoded = %+v, want %+v", decoded, restated)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestDefectAggregateFindingWireShapeMatchesTheArtifact pins the defect
// aggregate restatement (Goobers#4001) the same way.
//
// telemetryclient.Finding is a restatement of rollup.Finding, and the whole
// point of the plane is that a stage's artifact is the SAME document whether
// it was derived locally or answered by the daemon. A renamed tag or a
// dropped omitempty here would produce an artifact that no longer validates
// against candidate-findings-v1 — in a pod only, where nobody is looking.
func TestDefectAggregateFindingWireShapeMatchesTheArtifact(t *testing.T) {
	finding := rollup.Finding{
		Kind:      rollup.FindingCreditAssignment,
		Subject:   "stage:implement",
		Metrics:   map[string]float64{"failureShare": 0.75, "runs": 8},
		Threshold: 0.3,
		FlaggedRuns: []rollup.JournalPointer{
			{RunID: "run-3", Seq: 12},
			{RunID: "run-4"},
		},
		Signature:         "credit-assignment:stage:implement",
		Classification:    "workflow-or-gate",
		RecommendedAction: "workflow-or-gate",
		NominationGuardrails: &rollup.NominationGuardrails{
			DedupeKey:                  "credit:stage:implement",
			RequiresUpstreamCauseCheck: true,
			RequiresHumanReview:        false,
			GoverningTargetTreatment:   "advisory",
		},
	}
	wire := telemetryclient.Finding{
		Kind:      string(finding.Kind),
		Subject:   finding.Subject,
		Metrics:   finding.Metrics,
		Threshold: finding.Threshold,
		FlaggedRuns: []telemetryclient.JournalPointer{
			{RunID: "run-3", Seq: 12},
			{RunID: "run-4"},
		},
		Signature:         finding.Signature,
		Classification:    string(finding.Classification),
		RecommendedAction: finding.RecommendedAction,
		NominationGuardrails: &telemetryclient.NominationGuardrails{
			DedupeKey:                  finding.NominationGuardrails.DedupeKey,
			RequiresUpstreamCauseCheck: finding.NominationGuardrails.RequiresUpstreamCauseCheck,
			RequiresHumanReview:        finding.NominationGuardrails.RequiresHumanReview,
			GoverningTargetTreatment:   finding.NominationGuardrails.GoverningTargetTreatment,
		},
	}
	findingJSON, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var asFinding, asWire map[string]any
	if err := json.Unmarshal(findingJSON, &asFinding); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wireJSON, &asWire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asFinding, asWire) {
		t.Fatalf("the wire restatement drifted from the artifact shape\nfinding: %s\nwire:    %s", findingJSON, wireJSON)
	}
}

// TestDefectAggregatePromotionSignalWireShapeMatches pins the other
// restatement the artifact carries.
func TestDefectAggregatePromotionSignalWireShapeMatches(t *testing.T) {
	signal := readservice.PromotionSignal{
		Node:              "stage:implement",
		Value:             -0.2,
		Lower:             -0.3,
		Upper:             -0.1,
		Source:            "randomized",
		Caveat:            "randomized assignment",
		PromotionEligible: true,
	}
	wire := telemetryclient.PromotionSignal{
		Node:              signal.Node,
		Value:             signal.Value,
		Lower:             signal.Lower,
		Upper:             signal.Upper,
		Source:            signal.Source,
		Caveat:            signal.Caveat,
		PromotionEligible: signal.PromotionEligible,
	}
	signalJSON, err := json.Marshal(signal)
	if err != nil {
		t.Fatal(err)
	}
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if string(signalJSON) != string(wireJSON) {
		t.Fatalf("promotion signal restatement drifted\nread service: %s\nwire:         %s", signalJSON, wireJSON)
	}
}

// TestDefectAggregateCausalCreditWireShapeMatches pins the third restatement:
// the causal estimates the artifact's causalCredit array carries.
func TestDefectAggregateCausalCreditWireShapeMatches(t *testing.T) {
	estimate := readmodel.CausalNodeCredit{
		Node:              "stage:implement",
		Effect:            -0.2,
		Lower:             -0.3,
		Upper:             -0.1,
		Identification:    readmodel.CausalRandomized,
		Caveat:            "randomized assignment",
		TreatedBefore:     1,
		TreatedAfter:      10,
		ControlBefore:     2,
		ControlAfter:      9,
		IntervalAvailable: true,
		PromotionEligible: true,
		PromotionSource:   string(readmodel.CausalRandomized),
	}
	wire := telemetryclient.CausalNodeCredit{
		Node:              estimate.Node,
		Effect:            estimate.Effect,
		Lower:             estimate.Lower,
		Upper:             estimate.Upper,
		Identification:    string(estimate.Identification),
		Caveat:            estimate.Caveat,
		TreatedBefore:     estimate.TreatedBefore,
		TreatedAfter:      estimate.TreatedAfter,
		ControlBefore:     estimate.ControlBefore,
		ControlAfter:      estimate.ControlAfter,
		IntervalAvailable: estimate.IntervalAvailable,
		PromotionEligible: estimate.PromotionEligible,
		PromotionSource:   estimate.PromotionSource,
	}
	estimateJSON, err := json.Marshal(estimate)
	if err != nil {
		t.Fatal(err)
	}
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if string(estimateJSON) != string(wireJSON) {
		t.Fatalf("causal credit restatement drifted\nread model: %s\nwire:       %s", estimateJSON, wireJSON)
	}
}
