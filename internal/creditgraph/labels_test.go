package creditgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLabelTestRecord(t *testing.T, runDir, runID string) []byte {
	t.Helper()
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(RunRecord{Schema: RecordSchemaVersion, Status: RecordComplete, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, RecordFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func humanLabel(runID string, outcome LabelOutcome, at time.Time) GroundTruthLabel {
	return GroundTruthLabel{
		RunID: runID, Outcome: outcome, Reason: "checked by hand", Source: LabelSourceHuman,
		LabeledBy: "alice", LabeledAt: at, RecordedAt: at.Add(time.Minute),
	}
}

func TestRecordLabelPersistsProvenanceWithoutTouchingAttribution(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run-1")
	record := writeLabelTestRecord(t, runDir, "run-1")
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	first, err := RecordLabel(context.Background(), runDir, humanLabel("run-1", LabelIncorrect, at))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.ID, "label-") {
		t.Fatalf("label id = %q, want deterministic label- prefix", first.ID)
	}
	again := humanLabel("run-1", LabelIncorrect, at)
	again.RecordedAt = at.Add(time.Hour)
	duplicate, err := RecordLabel(context.Background(), runDir, again)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate != first {
		t.Fatalf("duplicate label = %+v, want existing %+v", duplicate, first)
	}
	if _, err := RecordLabel(context.Background(), runDir, humanLabel("run-1", LabelCorrect, at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	store, err := ReadLabelStore(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if store.Schema != LabelStoreSchemaVersion || store.RunID != "run-1" || len(store.Labels) != 2 {
		t.Fatalf("store = %+v, want two labels for run-1", store)
	}
	got := store.Labels[0]
	if got.Source != LabelSourceHuman || got.LabeledBy != "alice" || !got.LabeledAt.Equal(at) ||
		!got.RecordedAt.Equal(at.Add(time.Minute)) || got.Reason != "checked by hand" {
		t.Fatalf("stored label provenance = %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(runDir, RecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, record) {
		t.Fatalf("attribution record changed after labeling:\n%s\nwant\n%s", after, record)
	}
}

func TestRecordLabelRejectsUnrecordedRunsAndInvalidLabels(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	unenrolled := filepath.Join(t.TempDir(), "run-unenrolled")
	if err := os.MkdirAll(unenrolled, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordLabel(context.Background(), unenrolled, humanLabel("run-unenrolled", LabelCorrect, at)); err == nil ||
		!strings.Contains(err.Error(), "no Backprop attribution record") {
		t.Fatalf("unenrolled error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(unenrolled, LabelStoreFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unenrolled run label store stat = %v, want absent", err)
	}

	runDir := filepath.Join(t.TempDir(), "run-1")
	writeLabelTestRecord(t, runDir, "run-1")
	cases := map[string]GroundTruthLabel{
		"bad outcome": humanLabel("run-1", "maybe", at),
		"no labeler":  func() GroundTruthLabel { l := humanLabel("run-1", LabelCorrect, at); l.LabeledBy = " "; return l }(),
		"bad source":  func() GroundTruthLabel { l := humanLabel("run-1", LabelCorrect, at); l.Source = "oracle"; return l }(),
		"wrong run":   humanLabel("run-2", LabelCorrect, at),
	}
	for name, label := range cases {
		if _, err := RecordLabel(context.Background(), runDir, label); err == nil {
			t.Fatalf("%s: RecordLabel succeeded, want error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, LabelStoreFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid labels created a store: %v", err)
	}
}

func TestEffectiveLabelIsIndependentOfRecordingOrder(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	early := humanLabel("run-1", LabelCorrect, at)
	early.ID = labelID(early)
	late := humanLabel("run-1", LabelIncorrect, at.Add(48*time.Hour))
	late.ID = labelID(late)
	tieA := humanLabel("run-1", LabelCorrect, at.Add(48*time.Hour))
	tieA.ID = labelID(tieA)

	forward, _ := EffectiveLabel([]GroundTruthLabel{early, late, tieA})
	backward, _ := EffectiveLabel([]GroundTruthLabel{tieA, late, early})
	if forward != backward {
		t.Fatalf("effective label depends on order: %+v vs %+v", forward, backward)
	}
	if !forward.LabeledAt.Equal(at.Add(48 * time.Hour)) {
		t.Fatalf("effective label = %+v, want the latest verdict", forward)
	}
	if _, ok := EffectiveLabel(nil); ok {
		t.Fatal("EffectiveLabel(nil) reported a label")
	}
}

func TestAggregateAttributionEvidenceRescoresLateGroundTruthDeterministically(t *testing.T) {
	observations := []AttributionObservation{
		{RunID: "run-1", EffectiveVersion: "ev", Workload: "manual", Attribution: Attribution{RunID: "run-1"}},
		{RunID: "run-2", EffectiveVersion: "ev", Workload: "manual", Attribution: Attribution{RunID: "run-2"}},
		{RunID: "run-3", EffectiveVersion: "ev", Workload: "manual", Attribution: Attribution{RunID: "run-3"}},
	}
	before := AggregateAttributionEvidence(observations)
	if len(before) != 1 || before[0].GroundTruth != nil {
		t.Fatalf("unlabeled cohort = %+v, want no ground-truth summary", before)
	}

	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	correct := humanLabel("run-1", LabelCorrect, at)
	incorrect := humanLabel("run-2", LabelIncorrect, at.Add(72*time.Hour))
	observations[0].GroundTruth = &correct
	observations[1].GroundTruth = &incorrect
	after := AggregateAttributionEvidence(observations)
	want := GroundTruthSummary{LabeledRunCount: 2, CorrectRunCount: 1, IncorrectRunCount: 1}
	if len(after) != 1 || after[0].GroundTruth == nil || *after[0].GroundTruth != want || after[0].RunCount != 3 {
		t.Fatalf("labeled cohort = %+v, want %+v over three runs", after, want)
	}

	reversed := []AttributionObservation{observations[2], observations[1], observations[0]}
	again, err := json.Marshal(AggregateAttributionEvidence(reversed))
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatalf("re-scored aggregate depends on observation order:\n%s\n%s", first, again)
	}
}

func labeledCohort(shares []float64, outcomes []LabelOutcome) []AttributionObservation {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	observations := make([]AttributionObservation, 0, len(shares))
	for i, share := range shares {
		runID := "run-" + string(rune('a'+i))
		observation := AttributionObservation{
			RunID: runID, EffectiveVersion: "ev", Workload: "manual",
			Attribution: Attribution{RunID: runID, Contributions: []Contribution{{
				NodeID: "tool:edit", Stage: "implement", Path: []string{"stage:implement", "tool:edit"},
				Share: share, Confidence: 0.6,
			}}},
		}
		if outcomes[i] != "" {
			label := humanLabel(runID, outcomes[i], at)
			observation.GroundTruth = &label
		}
		observations = append(observations, observation)
	}
	return observations
}

func topPath(t *testing.T, observations []AttributionObservation) ContributingPath {
	t.Helper()
	cohorts := AggregateAttributionEvidence(observations)
	if len(cohorts) != 1 || len(cohorts[0].TopContributingPaths) != 1 {
		t.Fatalf("cohorts = %+v, want one cohort with one path", cohorts)
	}
	return cohorts[0].TopContributingPaths[0]
}

func assertNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestGroundTruthLabelsWeightCohortPathScores(t *testing.T) {
	shares := []float64{0.4, 0.4, 0.4}
	for _, tc := range []struct {
		name    string
		outcome LabelOutcome
		want    float64
	}{
		{name: "unlabeled", outcome: "", want: 0.6},
		{name: "correct", outcome: LabelCorrect, want: 0.8},
		{name: "incorrect", outcome: LabelIncorrect, want: 0.3},
	} {
		path := topPath(t, labeledCohort(shares, []LabelOutcome{tc.outcome, tc.outcome, tc.outcome}))
		assertNear(t, tc.name+" confidence", path.Confidence, tc.want)
		assertNear(t, tc.name+" share", path.Share, 0.4)
	}

	mixed := labeledCohort([]float64{0.2, 0.8}, []LabelOutcome{LabelCorrect, LabelIncorrect})
	path := topPath(t, mixed)
	assertNear(t, "mixed share", path.Share, (2*0.2+0.5*0.8)/2.5)
	assertNear(t, "mixed confidence", path.Confidence, (2*0.8+0.5*0.3)/2.5)
	reversed := topPath(t, []AttributionObservation{mixed[1], mixed[0]})
	if reversed.Share != path.Share || reversed.Confidence != path.Confidence {
		t.Fatalf("weighted path depends on observation order: %+v vs %+v", path, reversed)
	}
	unlabeled := topPath(t, labeledCohort([]float64{0.2, 0.8}, []LabelOutcome{"", ""}))
	assertNear(t, "unlabeled mixed share", unlabeled.Share, 0.5)
}

func TestGroundTruthLabelsWeightFaultAuditConfidence(t *testing.T) {
	audit := func(outcome LabelOutcome) FaultFinding {
		at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		var observations []AttributionObservation
		for _, runID := range []string{"run-1", "run-2", "run-3"} {
			observation := auditObservation(runID, "implementation", "version-a", "implement",
				"workflow selected an unsuitable tool", ClassBadToolChoice, 0.6, "stage:implement/tool:one")
			if outcome != "" {
				label := humanLabel(runID, outcome, at)
				observation.GroundTruth = &label
			}
			observations = append(observations, observation)
		}
		report := AuditFaultDomains(observations, FaultAuditConfig{Now: at.Add(time.Hour), Since: at.Add(-time.Hour)})
		if len(report.WorkflowFindings) != 1 {
			t.Fatalf("report = %+v, want one workflow finding", report)
		}
		return report.WorkflowFindings[0]
	}
	unlabeled, correct, incorrect := audit(""), audit(LabelCorrect), audit(LabelIncorrect)
	if unlabeled.Confidence != 0.6 || correct.Confidence != 0.8 || incorrect.Confidence != 0.3 {
		t.Fatalf("confidence unlabeled=%v correct=%v incorrect=%v, want 0.6/0.8/0.3",
			unlabeled.Confidence, correct.Confidence, incorrect.Confidence)
	}
	if again := audit(LabelIncorrect); again.ID != incorrect.ID || again.Confidence != incorrect.Confidence {
		t.Fatalf("labeled audit is not deterministic: %+v vs %+v", incorrect, again)
	}
}
