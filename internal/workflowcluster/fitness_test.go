package workflowcluster

import (
	"math"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func fitnessDescriptor(id, workflow, digest string, outcome journal.RunPhase, seconds, cost float64, features map[string]float64) BehaviorDescriptor {
	numeric := map[string]float64{
		"duration.log1pSeconds": math.Log1p(seconds),
		"usage.log1pCostCents":  math.Log1p(cost * 100),
		"gates.humanPaused":     0,
		"gates.escalated":       0,
		"visits.retry":          0,
		"visits.loop":           0,
		"retries.policy":        0,
		"retries.infra":         0,
		"retries.human":         0,
		"reruns":                0,
	}
	for name, value := range features {
		numeric[name] = value
	}
	return BehaviorDescriptor{
		RunID: id, Gaggle: "g", Workflow: workflow, WorkflowDigest: digest,
		Outcome: outcome, Numeric: numeric,
	}
}

func TestEvaluateFitnessMixedOutcomesAndDigestJoin(t *testing.T) {
	model := Model{Clusters: []Cluster{
		{ID: "c0", Members: []string{"g/a", "g/b"}},
		{ID: "c1", Members: []string{"g/c"}},
	}}
	scan := BehaviorScan{Candidates: 5, Descriptors: []BehaviorDescriptor{
		fitnessDescriptor("r1", "a", "sha256:a", journal.PhaseCompleted, 10, 1, nil),
		fitnessDescriptor("r2", "a", "sha256:a", journal.PhaseFailed, 20, 2, map[string]float64{"gates.humanPaused": 1}),
		fitnessDescriptor("r3", "b", "sha256:b", journal.PhaseEscalated, 30, 3, map[string]float64{"gates.escalated": 1, "visits.loop": 1}),
		fitnessDescriptor("old", "a", "sha256:old", journal.PhaseCompleted, 1, 0.1, nil),
		fitnessDescriptor("other", "z", "sha256:z", journal.PhaseCompleted, 1, 0.1, nil),
	}}

	got := EvaluateFitness(model, map[string]string{"g/a": "sha256:a", "g/b": "sha256:b", "g/c": "sha256:c"}, scan, 3)
	if len(got.Clusters) != 2 || got.Scan.Descriptors != nil {
		t.Fatalf("report = %+v", got)
	}
	first := got.Clusters[0]
	if first.RunCount != 3 || first.InsufficientRuns || first.SuccessRate != 1.0/3 ||
		first.MedianWallTimeSeconds != 20 || first.P90WallTimeSeconds != 30 ||
		math.Abs(first.MedianCostUSD-2) > 1e-9 || math.Abs(first.P90CostUSD-3) > 1e-9 ||
		first.HumanGateRate != 1.0/3 || first.EscalationRate != 1.0/3 || first.RetryLoopRate != 1.0/3 {
		t.Fatalf("mixed fitness = %+v", first)
	}
	if first.SuccessInterval.Lower <= 0 || first.SuccessInterval.Upper >= 1 {
		t.Fatalf("Wilson interval = %+v", first.SuccessInterval)
	}
	if empty := got.Clusters[1]; empty.ClusterID != "c1" || empty.RunCount != 0 || !empty.InsufficientRuns ||
		empty.MinimumRuns != 3 || empty.SuccessRate != 0 {
		t.Fatalf("no-run cluster fitness = %+v", empty)
	}
}

func TestEvaluateFitnessReportsClusterWithNoRuns(t *testing.T) {
	model := Model{Clusters: []Cluster{{ID: "c0", Members: []string{"g/a"}}}}
	got := EvaluateFitness(model, map[string]string{"g/a": "sha256:a"}, BehaviorScan{}, DefaultFitnessMinRuns)
	if len(got.Clusters) != 1 || got.Clusters[0].RunCount != 0 || !got.Clusters[0].InsufficientRuns ||
		got.Clusters[0].MinimumRuns != DefaultFitnessMinRuns {
		t.Fatalf("fitness = %+v", got)
	}
}

func TestEvaluateFitnessAveragesEvenSampleMedian(t *testing.T) {
	model := Model{Clusters: []Cluster{{ID: "c0", Members: []string{"g/a"}}}}
	scan := BehaviorScan{Descriptors: []BehaviorDescriptor{
		fitnessDescriptor("r1", "a", "sha256:a", journal.PhaseCompleted, 10, 1, nil),
		fitnessDescriptor("r2", "a", "sha256:a", journal.PhaseCompleted, 20, 2, nil),
	}}

	got := EvaluateFitness(model, map[string]string{"g/a": "sha256:a"}, scan, 2).Clusters[0]
	if math.Abs(got.MedianWallTimeSeconds-15) > 1e-9 || math.Abs(got.MedianCostUSD-1.5) > 1e-9 {
		t.Fatalf("even-sample medians = wall %v, cost %v", got.MedianWallTimeSeconds, got.MedianCostUSD)
	}
}
