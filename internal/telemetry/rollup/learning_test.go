package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
)

func TestLearningEpisodesProjectAndClusterNonPassReview(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "runs", "learning-run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "run.yaml"), []byte(`
schema: goobers.dev/journal/run/v1
runId: learning-run
workflow: implementation
workflowVersion: 1
workflowDigest: sha256:workflow
gooberDigest: sha256:goober
gaggle: web
startedAt: 2026-08-21T00:00:00Z
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2026-08-21T00:00:00Z","type":"run.started"}
{"schema":"goobers.dev/journal/event/v1","seq":2,"time":"2026-08-21T00:00:01Z","type":"gate.evaluated","gate":"review","verdict":"needs-changes","target":"implement","runner":{"failureSignature":"missing-test","correctionFeedback":"Add regression coverage.","findingIdentities":["finding-1"]}}
{"schema":"goobers.dev/journal/event/v1","seq":3,"time":"2026-08-21T00:00:02Z","type":"stage.started","stage":"implement","attempt":2}
{"schema":"goobers.dev/journal/event/v1","seq":4,"time":"2026-08-21T00:00:03Z","type":"gate.evaluated","gate":"review","verdict":"pass"}
{"schema":"goobers.dev/journal/event/v1","seq":5,"time":"2026-08-21T00:00:04Z","type":"run.finished","status":"completed"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{Gaggle: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	episode := episodes[0]
	if episode.Signature != "review|needs-changes|missing-test" ||
		episode.NextAttempt != 2 || episode.Outcome != learning.OutcomeFixed ||
		episode.EffectiveVersion == "" || len(episode.FindingIdentities) != 1 ||
		episode.FindingIdentities[0] != "finding-1" {
		t.Fatalf("episode = %+v", episode)
	}
	clusters, err := db.LearningClusters(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Count != 1 ||
		clusters[0].RecommendedAction != learning.ActionInstructionOrSkill {
		t.Fatalf("clusters = %+v", clusters)
	}
	if clusters[0].Episodes[0] != (JournalPointer{RunID: "learning-run", Seq: 2}) {
		t.Fatalf("cluster evidence = %+v, want source sequence", clusters[0].Episodes)
	}
}

func TestLearningEpisodesProjectValidationFailure(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "runs", "validation-run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "run.yaml"), []byte(`
schema: goobers.dev/journal/run/v1
runId: validation-run
workflow: implementation
workflowVersion: 1
gaggle: web
startedAt: 2026-08-21T00:00:00Z
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2026-08-21T00:00:00Z","type":"stage.started","stage":"local-ci","attempt":1}
{"schema":"goobers.dev/journal/event/v1","seq":2,"time":"2026-08-21T00:00:01Z","type":"stage.finished","stage":"local-ci","attempt":1,"status":"failure","error":{"code":"test_failed","message":"targeted test failed"}}
{"schema":"goobers.dev/journal/event/v1","seq":3,"time":"2026-08-21T00:00:02Z","type":"stage.started","stage":"local-ci","attempt":2}
{"schema":"goobers.dev/journal/event/v1","seq":4,"time":"2026-08-21T00:00:03Z","type":"stage.finished","stage":"local-ci","attempt":2,"status":"success"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	if episodes[0].Signature != "local-ci|validation||error|local-ci|targeted test failed" ||
		episodes[0].NextAttempt != 2 || episodes[0].Outcome != learning.OutcomeFixed {
		t.Fatalf("episode = %+v", episodes[0])
	}
}

func TestLearningEpisodesClassifyEveryOutcomeAndExcludeNonActionableClusters(t *testing.T) {
	root := t.TempDir()
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, outcome := range []string{
		learning.OutcomeFixed,
		learning.OutcomeRepeated,
		learning.OutcomeChangedFailure,
		learning.OutcomeEscalated,
		learning.OutcomeFalseFinding,
		learning.OutcomeUnresolved,
	} {
		run := writeLearningOutcomeRun(t, root, outcome)
		if err := db.IngestRun(context.Background(), run); err != nil {
			t.Fatalf("ingest %s: %v", outcome, err)
		}
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gotOutcomes := map[string]string{}
	for _, episode := range episodes {
		gotOutcomes[episode.RunID] = episode.Outcome
	}
	for _, want := range []string{
		learning.OutcomeFixed,
		learning.OutcomeRepeated,
		learning.OutcomeChangedFailure,
		learning.OutcomeEscalated,
		learning.OutcomeFalseFinding,
		learning.OutcomeUnresolved,
	} {
		if got := gotOutcomes["run-"+want]; got != want {
			t.Fatalf("run-%s outcome = %q, want %q; all=%v", want, got, want, gotOutcomes)
		}
	}

	clusters, err := db.LearningClusters(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	clustered := map[string]bool{}
	for _, cluster := range clusters {
		clustered[cluster.Signature] = true
	}
	for _, actionable := range []string{
		learning.OutcomeFixed,
		learning.OutcomeRepeated,
		learning.OutcomeChangedFailure,
		learning.OutcomeEscalated,
	} {
		if !clustered["signature-"+actionable] {
			t.Fatalf("actionable outcome %q missing from clusters: %+v", actionable, clusters)
		}
	}
	for _, excluded := range []string{learning.OutcomeFalseFinding, learning.OutcomeUnresolved} {
		if clustered["signature-"+excluded] {
			t.Fatalf("non-actionable outcome %q was clustered: %+v", excluded, clusters)
		}
	}
}

func TestDetectLearningEpisodesRequiresDistinctRunsAndRoutesCodeDefects(t *testing.T) {
	root := t.TempDir()
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, runID := range []string{"code-learning-1", "code-learning-2"} {
		run := writeFixedLearningRun(t, root, runID, "shared-code-signature", apiv1.LearningCodeDefect)
		if err := db.IngestRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	thresholds := DefaultThresholds()
	thresholds.MinLearningEpisodeRuns = 2
	findings, err := db.Detect(context.Background(), DetectRequest{
		StatsRequest: StatsRequest{Gaggle: "web"},
		Thresholds:   thresholds,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got *Finding
	for i := range findings {
		if findings[i].Kind == FindingLearningEpisode {
			got = &findings[i]
			break
		}
	}
	if got == nil || got.Signature != "shared-code-signature" ||
		got.Classification != apiv1.LearningCodeDefect ||
		got.RecommendedAction != learning.ActionCodeIssue ||
		got.Metrics["distinctRuns"] != 2 || len(got.FlaggedRuns) != 2 ||
		got.FlaggedRuns[0].RunID != "code-learning-2" ||
		got.FlaggedRuns[1].RunID != "code-learning-1" ||
		got.FlaggedRuns[0].Seq != 1 || got.FlaggedRuns[1].Seq != 1 {
		t.Fatalf("learning finding = %+v", got)
	}
}

func TestLearningClustersSeparateReclassifiedRoutes(t *testing.T) {
	root := t.TempDir()
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, fixture := range []struct {
		runID          string
		classification apiv1.LearningClassification
	}{
		{runID: "route-instruction-1", classification: apiv1.LearningInstruction},
		{runID: "route-instruction-2", classification: apiv1.LearningInstruction},
		{runID: "route-code-1", classification: apiv1.LearningCodeDefect},
		{runID: "route-code-2", classification: apiv1.LearningCodeDefect},
	} {
		run := writeFixedLearningRun(t, root, fixture.runID, "reclassified-signature", fixture.classification)
		if err := db.IngestRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}

	clusters, err := db.LearningClusters(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("clusters = %+v, want separate instruction and code routes", clusters)
	}
	routes := map[apiv1.LearningClassification]LearningCluster{}
	for _, cluster := range clusters {
		routes[cluster.Classification] = cluster
	}
	if got := routes[apiv1.LearningInstruction]; got.RunCount != 2 ||
		got.RecommendedAction != learning.ActionInstructionOrSkill {
		t.Fatalf("instruction route = %+v", got)
	}
	if got := routes[apiv1.LearningCodeDefect]; got.RunCount != 2 ||
		got.RecommendedAction != learning.ActionCodeIssue {
		t.Fatalf("code route = %+v", got)
	}
}

func TestLearningEpisodesProjectSameEvaluationDisproval(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "runs", "same-evaluation-disproval")
	writeLearningRunIdentity(t, run, "same-evaluation-disproval")
	finding := apiv1.Finding{
		ID: "finding-false", LearningSignature: "signature-false",
		LearningClassification: apiv1.LearningCodeDefect,
		Severity:               apiv1.SeverityError,
		Message:                "claimed source defect",
	}
	writeLearningEvents(t, run, []journal.Event{{
		Schema: journal.EventSchema, Seq: 1, Type: journal.EventGateEvaluated,
		Gate: "review", Verdict: string(apiv1.VerdictPass),
		Runner: map[string]any{
			"disprovenFindingIdentities": []string{finding.ID},
			"disprovenLearningFindings":  []apiv1.Finding{finding},
		},
	}})
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Outcome != learning.OutcomeFalseFinding ||
		episodes[0].FindingID != finding.ID {
		t.Fatalf("same-evaluation disproval episodes = %+v", episodes)
	}
	clusters, err := db.LearningClusters(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Fatalf("false finding became actionable cluster: %+v", clusters)
	}
}

func writeFixedLearningRun(
	t *testing.T,
	root, runID, signature string,
	classification apiv1.LearningClassification,
) string {
	t.Helper()
	run := filepath.Join(root, "runs", runID)
	writeLearningRunIdentity(t, run, runID)
	finding := apiv1.Finding{
		ID: "finding-" + runID, LearningSignature: signature,
		LearningClassification: classification,
		EvidenceDigest:         "sha256:" + runID,
		Severity:               apiv1.SeverityError,
		Message:                "shared code defect",
		Location:               "internal/widget.go",
	}
	writeLearningEvents(t, run, []journal.Event{
		{
			Schema: journal.EventSchema, Seq: 1, Time: time.Date(2026, 8, 21, 0, 0, 1, 0, time.UTC),
			Type: journal.EventGateEvaluated, Gate: "review",
			Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement",
			Runner: map[string]any{"learningFindings": []apiv1.Finding{finding}},
		},
		{
			Schema: journal.EventSchema, Seq: 2, Time: time.Date(2026, 8, 21, 0, 0, 2, 0, time.UTC),
			Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictPass),
		},
	})
	return run
}

func writeLearningOutcomeRun(t *testing.T, root, outcome string) string {
	t.Helper()
	runID := "run-" + outcome
	run := filepath.Join(root, "runs", runID)
	writeLearningRunIdentity(t, run, runID)

	sourceFinding := apiv1.Finding{
		ID: "finding-" + outcome, LearningSignature: "signature-" + outcome,
		LearningClassification: apiv1.LearningCodeDefect,
		EvidenceDigest:         "sha256:evidence-" + outcome,
		Severity:               apiv1.SeverityError,
		Message:                "durable defect " + outcome,
		Location:               "internal/widget.go",
	}
	events := []journal.Event{{
		Schema: journal.EventSchema, Seq: 1, Time: time.Date(2026, 8, 21, 0, 0, 1, 0, time.UTC),
		Type: journal.EventGateEvaluated, Gate: "review-" + outcome,
		Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement",
		Runner: map[string]any{
			"repassAttempt": 1, "learningFindings": []apiv1.Finding{sourceFinding},
		},
	}}
	switch outcome {
	case learning.OutcomeFixed:
		events = append(events, journal.Event{
			Schema: journal.EventSchema, Seq: 2, Type: journal.EventGateEvaluated,
			Gate: "review-" + outcome, Verdict: string(apiv1.VerdictPass),
		})
	case learning.OutcomeRepeated:
		events = append(events, journal.Event{
			Schema: journal.EventSchema, Seq: 2, Type: journal.EventGateEvaluated,
			Gate: "review-" + outcome, Verdict: string(apiv1.VerdictNeedsChanges),
			Runner: map[string]any{"learningFindings": []apiv1.Finding{sourceFinding}},
		})
	case learning.OutcomeChangedFailure:
		changed := sourceFinding
		changed.ID = "changed-finding"
		changed.LearningSignature = "changed-signature"
		events = append(events, journal.Event{
			Schema: journal.EventSchema, Seq: 2, Type: journal.EventGateEvaluated,
			Gate: "review-" + outcome, Verdict: string(apiv1.VerdictNeedsChanges),
			Runner: map[string]any{"learningFindings": []apiv1.Finding{changed}},
		})
	case learning.OutcomeFalseFinding:
		events = append(events, journal.Event{
			Schema: journal.EventSchema, Seq: 2, Type: journal.EventGateEvaluated,
			Gate: "review-" + outcome, Verdict: string(apiv1.VerdictPass),
			Runner: map[string]any{"disprovenFindingIdentities": []string{sourceFinding.ID}},
		})
	case learning.OutcomeEscalated:
		events = append(events, journal.Event{
			Schema: journal.EventSchema, Seq: 2, Type: journal.EventRunFinished,
			Status: string(journal.PhaseEscalated),
		})
	}
	writeLearningEvents(t, run, events)
	return run
}

func writeLearningRunIdentity(t *testing.T, run, runID string) {
	t.Helper()
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "run.yaml"), []byte(fmt.Sprintf(`
schema: goobers.dev/journal/run/v1
runId: %s
workflow: implementation
workflowVersion: 1
workflowDigest: sha256:workflow
gooberDigest: sha256:goober
gaggle: web
startedAt: 2026-08-21T00:00:00Z
`, runID)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLearningEvents(t *testing.T, run string, events []journal.Event) {
	t.Helper()
	var data []byte
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
