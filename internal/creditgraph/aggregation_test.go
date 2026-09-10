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
				Causes:        []CauseFinding{{NodeID: "node-a", Stage: "plan", Class: ClassBadToolResult, Evidence: []string{"intervention: stage plan attempt 1 failed and attempt 2 succeeded"}}, {NodeID: "node-b", Stage: "build", Class: ClassBadToolResult, Evidence: []string{"tool result failed"}}},
			},
			Evidence: []AttributionEvidenceLink{
				{RunID: "run-1", NodeID: "node-a", Stage: "plan", Detail: "intervention: stage plan attempt 1 failed and attempt 2 succeeded", Source: string(ClassBadToolResult), JournalSequence: 10, JournalPath: "gaggles/example/runs/run-1/events.jsonl"},
				{RunID: "run-1", NodeID: "node-b", Stage: "build", Detail: "tool result failed", Source: string(ClassBadToolResult), JournalSequence: 11, JournalPath: "gaggles/example/runs/run-1/events.jsonl"},
			},
		},
		{
			RunID:            "run-2",
			EffectiveVersion: "v1",
			Workload:         "implementation",
			Attribution: Attribution{
				Contributions: []Contribution{{NodeID: "node-a", Share: 0.6, Confidence: 0.7}, {NodeID: "node-c", Share: 0.4, Confidence: 0.75}},
				Causes:        []CauseFinding{{NodeID: "node-a", Stage: "plan", Class: ClassBadToolResult, Evidence: []string{"intervention: stage plan attempt 3 failed and attempt 4 succeeded"}}},
			},
			Evidence: []AttributionEvidenceLink{
				{RunID: "run-2", NodeID: "node-a", Stage: "plan", Detail: "intervention: stage plan attempt 3 failed and attempt 4 succeeded", Source: string(ClassBadToolResult), JournalSequence: 12, JournalPath: "gaggles/example/runs/run-2/events.jsonl"},
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
			Causes:        []CauseFinding{{NodeID: "tool-perf", Stage: "qa", Class: ClassBadToolResult, Evidence: []string{"failed tool result"}}},
		}, Evidence: []AttributionEvidenceLink{{
			RunID: "a", NodeID: "tool-perf", Stage: "qa", Detail: "failed tool result", Source: string(ClassBadToolResult), JournalSequence: 20, JournalPath: "gaggles/example/runs/a/events.jsonl",
		}}},
		{RunID: "b", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "tool-perf", Share: 0.2, Confidence: 0.95}},
			Causes:        []CauseFinding{{NodeID: "tool-perf", Stage: "qa", Class: ClassBadToolResult, Evidence: []string{"observed success in retry"}}},
		}, Evidence: []AttributionEvidenceLink{{
			RunID: "b", NodeID: "tool-perf", Stage: "qa", Detail: "observed success in retry", Source: string(ClassBadToolResult), JournalSequence: 21, JournalPath: "gaggles/example/runs/b/events.jsonl",
		}}},
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

func TestAggregateAttributionEvidencePreservesDistinctPaths(t *testing.T) {
	obs := []AttributionObservation{{
		RunID:            "run-1",
		EffectiveVersion: "v1",
		Workload:         "main",
		Attribution: Attribution{
			Contributions: []Contribution{
				{NodeID: "node-a", Path: []string{"root", "route-a", "node-a"}, Share: 0.7, Confidence: 0.8},
				{NodeID: "node-a", Path: []string{"root", "route-b", "node-a"}, Share: 0.3, Confidence: 0.6},
			},
		},
	}}

	got := AggregateAttributionEvidence(obs)
	if len(got) != 1 {
		t.Fatalf("cohorts = %d, want 1", len(got))
	}
	if len(got[0].TopContributingPaths) != 2 {
		t.Fatalf("top path count = %d, want 2 distinct paths", len(got[0].TopContributingPaths))
	}
	if got[0].TopContributingPaths[0].Nodes[0] != "root" {
		t.Fatalf("first path = %#v, want root-prefixed path", got[0].TopContributingPaths[0].Nodes)
	}
}

func TestAggregateAttributionEvidenceGroupsByVersionAndWorkload(t *testing.T) {
	obs := []AttributionObservation{
		{RunID: "run-1", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "node-a", Share: 0.7, Confidence: 0.8}},
		}},
		{RunID: "run-2", EffectiveVersion: "v1", Workload: "worker", Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "node-b", Share: 0.9, Confidence: 0.7}},
		}},
	}
	got := AggregateAttributionEvidence(obs)
	if len(got) != 2 {
		t.Fatalf("cohorts = %d, want 2 different workload cohorts", len(got))
	}
	if got[0].Workload != "main" || got[1].Workload != "worker" {
		t.Fatalf("cohort workloads = %q, %q, want main then worker", got[0].Workload, got[1].Workload)
	}
}

func TestAggregateAttributionEvidenceLinksConcreteRunEvidence(t *testing.T) {
	obs := []AttributionObservation{{
		RunID:            "run-42",
		EffectiveVersion: "v1",
		Workload:         "main",
		Attribution:      Attribution{Contributions: []Contribution{{NodeID: "node-a", Path: []string{"root", "node-a"}, Share: 0.5, Confidence: 0.8}}, Causes: []CauseFinding{{NodeID: "node-a", Stage: "plan", Class: ClassBadToolResult, Evidence: []string{"intervention: retry succeeded"}}}},
		Evidence: []AttributionEvidenceLink{
			{
				RunID:           "run-42",
				NodeID:          "node-a",
				Stage:           "plan",
				Detail:          "share=0.500000, confidence=0.800000",
				Source:          "contribution",
				JournalSequence: 12,
				JournalPath:     "gaggles/example/runs/run-42/events.jsonl",
				ArtifactPath:    "spans/sha256/aa/bb",
				ArtifactDigest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			{
				RunID:           "run-42",
				NodeID:          "node-a",
				Stage:           "plan",
				Detail:          "intervention: retry succeeded",
				Source:          string(ClassBadToolResult),
				JournalSequence: 14,
				JournalPath:     "gaggles/example/runs/run-42/events.jsonl",
				ArtifactPath:    "artifacts/sha256/cc/dd",
				ArtifactDigest:  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		},
	}}
	got := AggregateAttributionEvidence(obs)
	if len(got) != 1 || len(got[0].TopContributingPaths) != 1 {
		t.Fatalf("cohorts = %+v, want 1 cohort with 1 path", got)
	}
	link := got[0].TopContributingPaths[0].Evidence[0]
	if link.RunID != "run-42" || link.JournalPath != "gaggles/example/runs/run-42/events.jsonl" || link.ArtifactDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || link.JournalSequence != 12 {
		t.Fatalf("evidence link = %+v, want concrete run journal and artifact pointers", link)
	}
	if got[0].CounterEvidence[0].JournalPath != "gaggles/example/runs/run-42/events.jsonl" || got[0].CounterEvidence[0].ArtifactPath != "artifacts/sha256/cc/dd" {
		t.Fatalf("counter evidence = %+v, want concrete references", got[0].CounterEvidence[0])
	}
}

func TestAggregateAttributionEvidenceDoesNotFabricateConcreteLinks(t *testing.T) {
	obs := []AttributionObservation{{
		RunID:            "run-7",
		EffectiveVersion: "v1",
		Workload:         "main",
		Attribution: Attribution{
			Contributions: []Contribution{{NodeID: "node-a", Path: []string{"outcome", "run:run-7", "node-a"}, Share: 0.5, Confidence: 0.8}},
			Causes:        []CauseFinding{{NodeID: "node-a", Stage: "plan", Class: ClassBadToolResult, Evidence: []string{"tool result failed"}}},
		},
	}}

	got := AggregateAttributionEvidence(obs)
	if len(got) != 1 || len(got[0].TopContributingPaths) != 1 {
		t.Fatalf("cohorts = %+v, want 1 cohort with 1 path", got)
	}
	if got[0].TopContributingPaths[0].Evidence != nil {
		t.Fatalf("contribution evidence = %+v, want no fabricated concrete links", got[0].TopContributingPaths[0].Evidence)
	}
	if got[0].CounterEvidence != nil {
		t.Fatalf("counter evidence = %+v, want no fabricated concrete links", got[0].CounterEvidence)
	}
}
