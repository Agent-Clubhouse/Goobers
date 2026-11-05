package creditgraph

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestAggregateAttributionEvidencePreservesSignedScores(t *testing.T) {
	observations := []AttributionObservation{
		{RunID: "a", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{
				{NodeID: "positive", Share: 0.4, Score: 0.4, Confidence: 0.8},
				{NodeID: "negative", Share: 0.5, Score: -0.5, Confidence: 0.8},
				{NodeID: "neutral", Share: 0.1, Score: 0.1, Confidence: 0.8},
			},
		}},
		{RunID: "b", EffectiveVersion: "v1", Workload: "main", Attribution: Attribution{
			Contributions: []Contribution{
				{NodeID: "positive", Share: 0.6, Score: 0.6, Confidence: 0.8},
				{NodeID: "negative", Share: 0.3, Score: 0.3, Confidence: 0.8},
				{NodeID: "neutral", Share: 0.1, Score: -0.1, Confidence: 0.8},
			},
		}},
	}
	data, err := json.Marshal(AggregateAttributionEvidence(observations))
	if err != nil {
		t.Fatal(err)
	}
	var cohorts []struct {
		Paths []struct {
			Nodes []string `json:"nodes"`
			Share float64  `json:"share"`
			Score *float64 `json:"score"`
		} `json:"topContributingPaths"`
	}
	if err := json.Unmarshal(data, &cohorts); err != nil {
		t.Fatal(err)
	}
	if len(cohorts) != 1 || len(cohorts[0].Paths) != 3 {
		t.Fatalf("unexpected cohorts: %s", data)
	}
	expected := map[string][2]float64{
		"positive": {0.5, 0.5},
		"negative": {0.4, -0.1},
		"neutral":  {0.1, 0},
	}
	for _, path := range cohorts[0].Paths {
		want := expected[path.Nodes[len(path.Nodes)-1]]
		if path.Score == nil {
			t.Fatalf("signed score missing for %v; unsigned share must not stand in for impact", path.Nodes)
		}
		if math.Abs(path.Share-want[0]) > 1e-9 || math.Abs(*path.Score-want[1]) > 1e-9 {
			t.Errorf("path %v share=%g score=%g, want %v", path.Nodes, path.Share, *path.Score, want)
		}
	}
}

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

func TestAggregateAttributionEvidenceExcludesUndefinedEffectiveVersions(t *testing.T) {
	got := AggregateAttributionEvidence([]AttributionObservation{
		{RunID: "mixed-1", Workload: "implementation"},
		{RunID: "mixed-2", Workload: "implementation"},
		{RunID: "cohort", EffectiveVersion: "v1", Workload: "implementation"},
	})
	if len(got) != 1 || got[0].EffectiveVersion != "v1" || got[0].RunCount != 1 {
		t.Fatalf("cohorts = %+v, want only the defined effective version", got)
	}
}

func TestAggregateAttributionEvidenceExcludesFailedRecords(t *testing.T) {
	got := AggregateAttributionEvidence([]AttributionObservation{
		{
			RunID:            "failed",
			EffectiveVersion: "provisional",
			Workload:         "implementation",
			Status:           RecordFailed,
			Failure:          "span provenance unavailable",
		},
		{
			RunID:            "complete",
			EffectiveVersion: "v1",
			Workload:         "implementation",
			Status:           RecordComplete,
		},
	})
	if len(got) != 1 || got[0].EffectiveVersion != "v1" || got[0].RunCount != 1 {
		t.Fatalf("cohorts = %+v, want only the complete record", got)
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

func TestAggregateAttributionEvidenceGroupsProjectedRunPathsStructurally(t *testing.T) {
	first := projectedAggregationObservation(t, "run-a", "sha256:span-a")
	second := projectedAggregationObservation(t, "run-b", "sha256:span-b")

	got := AggregateAttributionEvidence([]AttributionObservation{first, second})
	if len(got) != 1 || got[0].RunCount != 2 {
		t.Fatalf("cohorts = %+v, want one two-run cohort", got)
	}
	if len(got[0].TopContributingPaths) != 1 {
		t.Fatalf("top paths = %+v, want one shared structural path", got[0].TopContributingPaths)
	}
	path := got[0].TopContributingPaths[0]
	joined := strings.Join(path.Nodes, "/")
	if strings.Contains(joined, "run-a") || strings.Contains(joined, "run-b") ||
		strings.Contains(joined, "span-a") || strings.Contains(joined, "span-b") {
		t.Fatalf("structural path = %q, want no run or span identity", joined)
	}
	if joined != "outcome/run/stage:verify/subagent:root/model:gpt-5" {
		t.Fatalf("structural path = %q", joined)
	}
	if len(path.Evidence) != 2 {
		t.Fatalf("evidence = %+v, want one exact reference per run", path.Evidence)
	}
	if path.Evidence[0].RunID == path.Evidence[1].RunID ||
		path.Evidence[0].JournalPath == path.Evidence[1].JournalPath ||
		path.Evidence[0].ArtifactDigest == path.Evidence[1].ArtifactDigest {
		t.Fatalf("evidence = %+v, want distinct run, journal, and artifact references", path.Evidence)
	}
}

func projectedAggregationObservation(t *testing.T, runID, spanDigest string) AttributionObservation {
	t.Helper()
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "verify", Attempt: 1},
		agentEvent("root", "", "verify", "gpt-5", journal.AgentStarted),
		{
			Seq: 3, Type: journal.EventSpanRecorded, Stage: "verify", Attempt: 1,
			Name: "verify.transcript", DataSchema: telemetry.GenAIEventSchema,
			Ref: &journal.Ref{Path: "spans/" + spanDigest, Digest: spanDigest},
		},
		{Seq: 4, Type: journal.EventRunnerAnnotation, Stage: "verify", Runner: map[string]any{
			SpanProvenanceKeyKind:    SpanProvenanceAnnotation,
			SpanProvenanceKeyAgentID: "root",
			SpanProvenanceKeyDigest:  spanDigest,
		}},
		{Seq: 5, Type: journal.EventStageFinished, Stage: "verify", Attempt: 1, Status: "success"},
		{Seq: 6, Type: journal.EventRunFinished, Status: "completed"},
	}
	graph, err := Build(Input{
		RunID: runID, Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{spanDigest: transcript(
			`{"role":"assistant","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":3}}`,
		)},
	})
	if err != nil {
		t.Fatalf("Build(%s): %v", runID, err)
	}
	attribution := Attribute(graph)
	var evidence []AttributionEvidenceLink
	for _, contribution := range attribution.Contributions {
		if contribution.Kind != KindModelInvocation {
			continue
		}
		evidence = append(evidence, AttributionEvidenceLink{
			RunID: runID, NodeID: contribution.NodeID, Stage: contribution.Stage,
			Detail:          "share=" + formatFloat(contribution.Share) + ", confidence=" + formatFloat(contribution.Confidence),
			Source:          "contribution",
			JournalSequence: 3,
			JournalPath:     "gaggles/example/runs/" + runID + "/events.jsonl",
			ArtifactPath:    "spans/" + spanDigest,
			ArtifactDigest:  spanDigest,
		})
	}
	return AttributionObservation{
		RunID: runID, EffectiveVersion: "v1", Workload: "implementation",
		Attribution: attribution, Evidence: evidence,
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

func TestAggregateAttributionEvidencePrefersConcreteMutationPaths(t *testing.T) {
	obs := []AttributionObservation{{
		RunID:            "run-8",
		EffectiveVersion: "v1",
		Workload:         "main",
		Attribution: Attribution{
			Contributions: []Contribution{
				{
					NodeID: "stage:implement#1",
					Path: []string{
						"outcome:@abort",
						"run:run-8",
						"stage:implement#1",
					},
					Kind:       KindStage,
					Stage:      "implement",
					Share:      1,
					Confidence: 0.6,
				},
				{
					NodeID: "model:root@sha256:span#1",
					Path: []string{
						"outcome:@abort",
						"run:run-8",
						"stage:implement#1",
						"subagent:root",
						"model:root@sha256:span#1",
					},
					Kind:       KindModelInvocation,
					Stage:      "implement",
					Share:      1,
					Confidence: 0.6,
				},
			},
		},
		Evidence: []AttributionEvidenceLink{
			{
				RunID:           "run-8",
				NodeID:          "stage:implement#1",
				Stage:           "implement",
				Detail:          "share=1.000000, confidence=0.600000",
				Source:          "contribution",
				JournalSequence: 7,
				JournalPath:     "gaggles/example/runs/run-8/events.jsonl",
			},
			{
				RunID:             "run-8",
				NodeID:            "model:root@sha256:span#1",
				Stage:             "implement",
				Detail:            "share=1.000000, confidence=0.600000",
				Source:            "contribution",
				JournalSequence:   5,
				JournalPath:       "gaggles/example/runs/run-8/events.jsonl",
				ArtifactPath:      "spans/sha256/aa/bb",
				ArtifactDigest:    "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				ArtifactMediaType: "application/jsonl",
			},
		},
	}}

	got := AggregateAttributionEvidence(obs)
	if len(got) != 1 || len(got[0].TopContributingPaths) != 1 {
		t.Fatalf("cohorts = %+v, want one surfaced concrete path", got)
	}
	path := got[0].TopContributingPaths[0]
	if lastNode(path.Nodes) != "model" {
		t.Fatalf("top path = %+v, want stable model identity ahead of structural stage path", path)
	}
	if len(path.Evidence) != 1 || path.Evidence[0].ArtifactDigest == "" {
		t.Fatalf("top path evidence = %+v, want exact artifact-backed evidence", path.Evidence)
	}
}

type projectedRunSpec struct {
	runID, spanDigest, runtimeDigest string
	attributedAgent                  bool
	transcript                       []string
}

func buildProjectedRun(t *testing.T, spec projectedRunSpec) (Attribution, []journal.Event) {
	t.Helper()
	runtimeSpan, err := json.Marshal(telemetry.SpanRecord{
		Schema: telemetry.SpanSchema,
		Attributes: map[string]string{
			telemetry.AttrHarnessVersion: "copilot-cli/1.0.86",
			"deployment.environment":     "production",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "verify", Attempt: 1},
	}
	if spec.attributedAgent {
		events = append(events, agentEvent("root", "", "verify", "gpt-5", journal.AgentStarted))
	}
	events = append(events,
		journal.Event{
			Seq: 3, Type: journal.EventSpanRecorded, Stage: "verify", Attempt: 1,
			Name: "verify.transcript", DataSchema: telemetry.GenAIEventSchema,
			Ref: &journal.Ref{Path: "spans/" + spec.spanDigest, Digest: spec.spanDigest},
		},
		journal.Event{
			Seq: 4, Type: journal.EventSpanRecorded, Stage: "verify", Attempt: 1,
			Name: "verify.runtime", DataSchema: telemetry.SpanSchema,
			Ref: &journal.Ref{Path: "spans/" + spec.runtimeDigest, Digest: spec.runtimeDigest},
		},
	)
	if spec.attributedAgent {
		events = append(events, journal.Event{Seq: 5, Type: journal.EventRunnerAnnotation, Stage: "verify", Runner: map[string]any{
			SpanProvenanceKeyKind:    SpanProvenanceAnnotation,
			SpanProvenanceKeyAgentID: "root",
			SpanProvenanceKeyDigest:  spec.spanDigest,
		}})
	}
	events = append(events,
		journal.Event{Seq: 6, Type: journal.EventStageFinished, Stage: "verify", Attempt: 1, Status: "success"},
		journal.Event{Seq: 7, Type: journal.EventRunFinished, Status: "completed"},
	)
	graph, err := Build(Input{
		RunID: spec.runID, Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{
			spec.spanDigest:    transcript(spec.transcript...),
			spec.runtimeDigest: runtimeSpan,
		},
	})
	if err != nil {
		t.Fatalf("Build(%s): %v", spec.runID, err)
	}
	return Attribute(graph), events
}

// structuralKeysByKind returns the cross-run aggregate key of every surfaced
// contribution in one projected run, grouped by node kind.
func structuralKeysByKind(attribution Attribution) map[NodeKind][]string {
	identities := contributionNodeIdentities(attribution.Contributions)
	keys := map[NodeKind][]string{}
	for _, contribution := range attribution.Contributions {
		if !surfaceContributionPath(contribution) {
			continue
		}
		path := structuralContributionPath(contributionPath(contribution), identities)
		keys[contribution.Kind] = append(keys[contribution.Kind], strings.Join(path, "/"))
	}
	for kind := range keys {
		sort.Strings(keys[kind])
	}
	return keys
}

func TestAggregateStructuralKeysAreStableAcrossProjectedRuns(t *testing.T) {
	toolTranscript := func(success bool) []string {
		return []string{
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-1","success":` + strconv.FormatBool(success) + `}}`,
		}
	}
	first, _ := buildProjectedRun(t, projectedRunSpec{
		runID: "run-a", spanDigest: "sha256:span-a", runtimeDigest: "sha256:rt-a", transcript: toolTranscript(true),
	})
	second, _ := buildProjectedRun(t, projectedRunSpec{
		runID: "run-b", spanDigest: "sha256:span-b", runtimeDigest: "sha256:rt-b", transcript: toolTranscript(true),
	})
	failed, _ := buildProjectedRun(t, projectedRunSpec{
		runID: "run-c", spanDigest: "sha256:span-c", runtimeDigest: "sha256:rt-c", transcript: toolTranscript(false),
	})
	firstKeys, secondKeys, failedKeys := structuralKeysByKind(first), structuralKeysByKind(second), structuralKeysByKind(failed)

	for _, kind := range []NodeKind{KindRuntime, KindEnvironment, KindToolCall, KindToolResult} {
		if len(firstKeys[kind]) == 0 {
			t.Fatalf("run-a has no surfaced %s contribution; keys = %v", kind, firstKeys)
		}
		if strings.Join(firstKeys[kind], "|") != strings.Join(secondKeys[kind], "|") {
			t.Errorf("%s keys differ across equivalent runs: %v vs %v", kind, firstKeys[kind], secondKeys[kind])
		}
	}
	for kind, keys := range firstKeys {
		for _, key := range keys {
			for _, runSpecific := range []string{"run-a", "span-a", "rt-a", "#1"} {
				if strings.Contains(key, runSpecific) {
					t.Errorf("%s key %q retains occurrence identity %q", kind, key, runSpecific)
				}
			}
		}
	}
	if got := firstKeys[KindRuntime][0]; !strings.HasSuffix(got, "/runtime:copilot-cli/1.0.86") {
		t.Errorf("runtime identity = %q, want harness label preserved", got)
	}
	if got := firstKeys[KindEnvironment][0]; !strings.HasSuffix(got, "/environment:production") {
		t.Errorf("environment identity = %q, want deployment label preserved", got)
	}
	if !strings.HasSuffix(firstKeys[KindToolResult][0], "toolresult:succeeded") ||
		!strings.HasSuffix(failedKeys[KindToolResult][0], "toolresult:failed") {
		t.Errorf("tool result keys = %v / %v, want succeeded and failed kept distinct",
			firstKeys[KindToolResult], failedKeys[KindToolResult])
	}
	if !strings.Contains(firstKeys[KindToolCall][0], "subagent:unknown/stage:verify/") {
		t.Errorf("tool call key = %q, want unattributed agent without its attempt suffix", firstKeys[KindToolCall][0])
	}
}

func TestAggregateAttributionEvidenceWeighsRepeatedOccurrencesOncePerRun(t *testing.T) {
	model := `{"role":"assistant","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":3}}`
	repeated, _ := buildProjectedRun(t, projectedRunSpec{
		runID: "run-a", spanDigest: "sha256:span-a", runtimeDigest: "sha256:rt-a",
		attributedAgent: true, transcript: []string{model, model, model},
	})
	single, _ := buildProjectedRun(t, projectedRunSpec{
		runID: "run-b", spanDigest: "sha256:span-b", runtimeDigest: "sha256:rt-b",
		attributedAgent: true, transcript: []string{model},
	})
	repeatedShare, repeatedCount, repeatedLinks := modelContributionEvidence(repeated, "run-a")
	singleShare, _, singleLinks := modelContributionEvidence(single, "run-b")
	if repeatedCount != 3 {
		t.Fatalf("repeated run has %d model invocations, want 3", repeatedCount)
	}
	got := AggregateAttributionEvidence([]AttributionObservation{
		{RunID: "run-a", EffectiveVersion: "v1", Workload: "implementation", Attribution: repeated, Evidence: repeatedLinks},
		{RunID: "run-b", EffectiveVersion: "v1", Workload: "implementation", Attribution: single, Evidence: singleLinks},
	})
	if len(got) != 1 {
		t.Fatalf("cohorts = %+v, want one", got)
	}
	var path *ContributingPath
	for i := range got[0].TopContributingPaths {
		if lastNode(got[0].TopContributingPaths[i].Nodes) == "model:gpt-5" {
			path = &got[0].TopContributingPaths[i]
		}
	}
	if path == nil {
		t.Fatalf("paths = %+v, want a shared model path", got[0].TopContributingPaths)
	}
	// Each run counts once: the repeated run's three occurrences are summed
	// into one run-level share before the two runs are averaged.
	want := (repeatedShare + singleShare) / 2
	if math.Abs(path.Share-want) > 1e-9 {
		t.Fatalf("share = %v, want per-run mean %v (repeated=%v, single=%v)", path.Share, want, repeatedShare, singleShare)
	}
	if len(path.Evidence) != 4 {
		t.Fatalf("evidence = %d links, want every occurrence retained (4)", len(path.Evidence))
	}
}

func modelContributionEvidence(attribution Attribution, runID string) (float64, int, []AttributionEvidenceLink) {
	var total float64
	var links []AttributionEvidenceLink
	for _, contribution := range attribution.Contributions {
		if contribution.Kind != KindModelInvocation {
			continue
		}
		total += contribution.Share
		links = append(links, AttributionEvidenceLink{
			RunID: runID, NodeID: contribution.NodeID, Stage: contribution.Stage,
			Detail: "share=" + formatFloat(contribution.Share) + ", confidence=" + formatFloat(contribution.Confidence),
			Source: "contribution", JournalPath: "gaggles/example/runs/" + runID + "/events.jsonl",
		})
	}
	return total, len(links), links
}
