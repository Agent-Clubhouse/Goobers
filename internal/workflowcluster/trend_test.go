package workflowcluster

import (
	"math"
	"testing"
	"time"
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
