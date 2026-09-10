package creditgraph

import "testing"

func TestAggregateAttributionEvidenceByCohort(t *testing.T) {
	obs := []AttributionObservation{
		{
			RunID:            "run-1",
			EffectiveVersion: "v1",
			Workload:         "implementation",
			Attribution: Attribution{
				Contributions: []Contribution{{NodeID: "node-a", Share: 0.7, Confidence: 0.8}, {NodeID: "node-b", Share: 0.3, Confidence: 0.6}},
				Causes:        []CauseFinding{{NodeID: "node-a", Stage: "plan", Evidence: []string{"intervention: stage plan attempt 1 failed and attempt 2 succeeded"}}, {NodeID: "node-b", Stage: "build", Evidence: []string{"tool result failed"}}},
			},
		},
		{
			RunID:            "run-2",
			EffectiveVersion: "v1",
			Workload:         "implementation",
			Attribution: Attribution{
				Contributions: []Contribution{{NodeID: "node-a", Share: 0.6, Confidence: 0.7}, {NodeID: "node-c", Share: 0.4, Confidence: 0.75}},
				Causes:        []CauseFinding{{NodeID: "node-a", Stage: "plan", Evidence: []string{"intervention: stage plan attempt 3 failed and attempt 4 succeeded"}}},
			},
		},
		{
			RunID:            "run-3",
			EffectiveVersion: "v2",
			Workload:         "implementation",
			Attribution: Attribution{
				Contributions: []Contribution{{NodeID: "node-x", Share: 0.9, Confidence: 0.95}},
			},
		},
	}

	got := AggregateAttributionEvidence(obs)
	if len(got) != 2 {
		t.Fatalf("cohorts = %d, want 2", len(got))
	}
	if got[0].EffectiveVersion != "v1" {
		t.Fatalf("first cohort version = %q, want v1", got[0].EffectiveVersion)
	}
	if got[0].RunCount != 2 {
		t.Fatalf("cohort run count = %d, want 2", got[0].RunCount)
	}
	if len(got[0].TopContributingPaths) < 2 {
		t.Fatalf("top paths = %#v, want at least two paths", got[0].TopContributingPaths)
	}
	if len(got[0].CounterEvidence) == 0 {
		t.Fatal("counter evidence missing from v1 cohort")
	}
	if got[1].EffectiveVersion != "v2" {
		t.Fatalf("second cohort version = %q, want v2", got[1].EffectiveVersion)
	}
}

func TestAggregateAttributionEvidencePreservesMixedConfidence(t *testing.T) {
	obs := []AttributionObservation{
		{RunID: "a", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "tool-perf", Share: 0.8, Confidence: 0.55}},
			Causes:        []CauseFinding{{NodeID: "tool-perf", Stage: "qa", Evidence: []string{"failed tool result"}}},
		}},
		{RunID: "b", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "tool-perf", Share: 0.2, Confidence: 0.95}},
			Causes:        []CauseFinding{{NodeID: "tool-perf", Stage: "qa", Evidence: []string{"observed success in retry"}}},
		}},
	}
	got := AggregateAttributionEvidence(obs)
	if len(got) != 1 {
		t.Fatalf("cohorts = %d, want 1", len(got))
	}
	if len(got[0].TopContributingPaths) != 1 {
		t.Fatalf("top path count = %d, want 1", len(got[0].TopContributingPaths))
	}
	path := got[0].TopContributingPaths[0]
	if path.Share <= 0 || path.Confidence <= 0 {
		t.Fatalf("path = %+v, want positive share and confidence", path)
	}
	if len(got[0].CounterEvidence) != 2 {
		t.Fatalf("counter evidence = %d, want 2", len(got[0].CounterEvidence))
	}
}
