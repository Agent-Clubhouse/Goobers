package workflowcluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

func TestAnalyzeTrendsUsesFrozenAssignmentsAndReportsSampledDrift(t *testing.T) {
	model := Model{
		ID: "sha256:model", FeatureVersion: StructuralFeatureVersion,
		Schema: Schema{Features: []SchemaFeature{{Name: "x", Kind: KindNumeric, Weight: 1, Min: 0, Max: 10}}},
		Clusters: []Cluster{
			{ID: "c0", Medoid: Point{ID: "left", Numeric: map[string]float64{"x": 0}}, Radius: 0.25},
			{ID: "c1", Medoid: Point{ID: "right", Numeric: map[string]float64{"x": 10}}, Radius: 0.25},
		},
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	observation := func(id, version string, day, x int, fitness float64) TrendObservation {
		return TrendObservation{
			ID: id, At: at.AddDate(0, 0, day), Point: Point{ID: id, Numeric: map[string]float64{"x": float64(x)}},
			Fitness: fitness, Versions: map[string]string{
				TrendAxisModel: version, TrendAxisHarness: "h1", TrendAxisAuthoringSkill: "sha256:skill",
			},
		}
	}
	report, err := AnalyzeTrends(model, []TrendObservation{
		observation("a", "m1", 0, 0, 1), observation("b", "m1", 0, 10, 1),
		observation("c", "m2", 1, 0, 1), observation("d", "m2", 1, 1, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ModelID != model.ID || report.Samples != 4 || len(report.Axes) != 3 {
		t.Fatalf("report = %+v", report)
	}
	modelAxis := report.Axes[0]
	if modelAxis.Axis != TrendAxisModel || len(modelAxis.Cohorts) != 2 {
		t.Fatalf("model axis = %+v", modelAxis)
	}
	first, second := modelAxis.Cohorts[0], modelAxis.Cohorts[1]
	if first.Samples != 2 || len(first.Occupancy) != 2 || math.Abs(first.Entropy-1) > 1e-9 || math.Abs(first.MeanPairwiseDistance-1) > 1e-9 {
		t.Fatalf("first cohort = %+v", first)
	}
	if second.Drift == nil || second.Drift.From != "m1" || second.Drift.Signal != "mode-collapse" ||
		math.Abs(second.Drift.OccupancyDistance-0.5) > 1e-9 || second.Samples != 2 ||
		len(second.Occupancy) != 1 || second.Occupancy[0].Samples != 2 || second.Occupancy[0].MeanFitness != 0.5 {
		t.Fatalf("second cohort = %+v", second)
	}
}

func TestAnalyzeTrendsRejectsMismatchedModel(t *testing.T) {
	_, err := AnalyzeTrends(Model{FeatureVersion: "other"}, []TrendObservation{{
		ID: "x", Point: Point{ID: "x"}, Versions: map[string]string{TrendAxisModel: "m"},
	}})
	if err == nil {
		t.Fatal("expected feature-version mismatch")
	}
}

func TestAnalyzeTrendsRejectsInvalidObservations(t *testing.T) {
	model := Model{FeatureVersion: StructuralFeatureVersion}
	for name, observations := range map[string][]TrendObservation{
		"missing id": {{Point: Point{ID: "point"}}},
		"duplicate id": {
			{ID: "same", Point: Point{ID: "a"}},
			{ID: "same", Point: Point{ID: "b"}},
		},
		"invalid fitness": {{ID: "x", Point: Point{ID: "x"}, Fitness: 2}},
	} {
		if _, err := AnalyzeTrends(model, observations); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func trendDigest(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)[:64]
}

// writeTrendParent records a terminal parent run whose stages start in order
// and whose task spans carry the given model per stage ("" records no span).
// It returns each stage's occurrence.
func writeTrendParent(t *testing.T, root, runID, gooberDigest string, stages [][2]string) []string {
	t.Helper()
	runsDir := instance.NewLayout(root).ForGaggle("g").RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Gaggle: "g", Workflow: "parent", WorkflowVersion: 1, WorkflowDigest: trendDigest("a"),
		GooberDigest: gooberDigest, Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage[0], Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var occurrences []string
	for _, event := range events {
		if event.Type != journal.EventStageStarted {
			continue
		}
		occurrences = append(occurrences, journal.StageOccurrence(runID, event, ""))
		model := stages[len(occurrences)-1][1]
		if model == "" {
			continue
		}
		data, err := json.Marshal(telemetry.SpanRecord{
			Schema: telemetry.SpanSchema, Kind: telemetry.SpanKindTask, StartTime: event.Time,
			Attributes: map[string]string{
				telemetry.AttrStage: event.Stage, telemetry.AttrBranch: "0", telemetry.AttrAttemptNumber: "1",
				telemetry.AttrModel: model, telemetry.AttrHarnessVersion: "h1",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.RecordSpanWithSchema(event.Stage, "task", telemetry.SpanSchema, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Disposition: "produced"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return occurrences
}

// writeTrendChild records a terminal agent-authored child of parentRunID
// pinned to a one-task definition.
func writeTrendChild(t *testing.T, root, runID, parentRunID, occurrence string) {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "child", Version: 1,
		Spec: apiv1.WorkflowSpec{Gaggle: "g", Start: "implement", Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "goal", Run: &apiv1.DeterministicRun{Command: []string{"true"}},
		}}},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(instance.NewLayout(root).ForGaggle("g").RunsDir(), journal.RunIdentity{
		RunID: runID, Gaggle: "g", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), GooberDigest: trendDigest("b"), ConfigGeneration: trendDigest("c"),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		Child: &journal.ChildLineage{
			Gaggle: "g", ParentRunID: parentRunID, ParentWorkflow: "parent", StageOccurrence: occurrence,
			InvocationKey: "author", AcceptanceID: "trigger-" + runID, SourceDigest: trendDigest("d"), EnvelopeDigest: trendDigest("e"),
		},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Disposition: "produced"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func trendAxis(t *testing.T, report TrendReport, name string) TrendAxis {
	t.Helper()
	for _, axis := range report.Axes {
		if axis.Axis == name {
			return axis
		}
	}
	t.Fatalf("axis %q missing from %+v", name, report)
	return TrendAxis{}
}

func TestScanChildTrendsAttributesAuthoringStageProvenance(t *testing.T) {
	root := t.TempDir()
	// A model upgrade also changes the run-wide goober digest; it must not
	// surface as an authoring-skill change. parent-1 mixes models across
	// stages, but only the authoring stage's model is attributed.
	first := writeTrendParent(t, root, "parent-1", trendDigest("1"), [][2]string{{"plan", "m1"}, {"review", "reviewer"}, {"draft", ""}})
	second := writeTrendParent(t, root, "parent-2", trendDigest("2"), [][2]string{{"plan", "m2"}})
	writeTrendChild(t, root, "child-1", "parent-1", first[0])
	writeTrendChild(t, root, "child-2", "parent-2", second[0])
	writeTrendChild(t, root, "child-3", "parent-1", first[2])
	writeTrendChild(t, root, "child-orphan", "parent-gone", first[0])

	report, err := ScanChildTrends(context.Background(), root, Model{FeatureVersion: StructuralFeatureVersion}, TrendScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Samples != 3 || report.Excluded != 1 || report.Candidates != 6 || report.OverLimit != 0 {
		t.Fatalf("report = %+v", report)
	}
	model := trendAxis(t, report, TrendAxisModel)
	if len(model.Cohorts) != 2 || model.Cohorts[0].Samples+model.Cohorts[1].Samples != 2 || model.MissingProvenance != 1 {
		t.Fatalf("model axis = %+v", model)
	}
	for _, cohort := range model.Cohorts {
		if cohort.Version != "m1" && cohort.Version != "m2" {
			t.Fatalf("model cohort %q is not an authoring-stage model", cohort.Version)
		}
	}
	if harness := trendAxis(t, report, TrendAxisHarness); len(harness.Cohorts) != 1 || harness.Cohorts[0].Samples != 2 || harness.MissingProvenance != 1 {
		t.Fatalf("harness axis = %+v", harness)
	}
	if skill := trendAxis(t, report, TrendAxisAuthoringSkill); len(skill.Cohorts) != 0 || skill.MissingProvenance != 3 {
		t.Fatalf("authoring-skill axis = %+v", skill)
	}
}

func TestScanChildTrendsIsBoundedAndCancellable(t *testing.T) {
	root := t.TempDir()
	occurrences := writeTrendParent(t, root, "parent-1", trendDigest("1"), [][2]string{{"plan", "m1"}})
	writeTrendChild(t, root, "child-1", "parent-1", occurrences[0])
	writeTrendChild(t, root, "child-2", "parent-1", occurrences[0])
	model := Model{FeatureVersion: StructuralFeatureVersion}

	capped, err := ScanChildTrends(context.Background(), root, model, TrendScanOptions{MaxRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if capped.Candidates != 3 || capped.OverLimit != 2 || capped.Samples+capped.Excluded > 1 {
		t.Fatalf("capped report = %+v", capped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanChildTrends(ctx, root, model, TrendScanOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan err = %v", err)
	}
}

func TestAuthoringVersionsSeparatesLoopVisits(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	parent := BehaviorInput{
		Identity: journal.RunIdentity{RunID: "parent"},
		Events: []journal.Event{
			{Seq: 1, Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Time: at},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Time: at.Add(time.Hour)},
		},
	}
	for i, model := range []string{"m1", "m2"} {
		parent.Spans = append(parent.Spans, telemetry.SpanRecord{
			Kind: telemetry.SpanKindTask, StartTime: at.Add(time.Duration(i)*time.Hour + time.Second),
			Attributes: map[string]string{
				telemetry.AttrStage: "plan", telemetry.AttrBranch: "0", telemetry.AttrAttemptNumber: "1",
				telemetry.AttrModel: model, telemetry.AttrHarnessVersion: "h1",
			},
		})
	}
	second := journal.StageOccurrence("parent", parent.Events[1], "")
	if model, harness := authoringVersions(parent, second); model != "m2" || harness != "h1" {
		t.Fatalf("second visit = %q, %q", model, harness)
	}
}
