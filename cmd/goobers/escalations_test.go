package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflow"
)

func TestEscalationsListsOnlyEscalatedRuns(t *testing.T) {
	root := t.TempDir()
	createEscalationInspectionRun(t, root, "escalated-run")

	completed := newTraceTestRun(t, root, "completed-run")
	if err := completed.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := completed.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "escalations", root)
	if code != 0 {
		t.Fatalf("escalations: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"escalated-run",
		"implementation",
		"gate/review",
		"repass budget exhausted",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("escalations stdout missing %q: %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "completed-run") {
		t.Fatalf("escalations included completed run: %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "escalations", "--json", root)
	if code != 0 {
		t.Fatalf("escalations --json: code = %d, stderr = %q", code, stderr)
	}
	var got escalationListResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("escalations --json produced invalid JSON: %v\n%s", err, stdout)
	}
	if len(got.Escalations) != 1 ||
		got.Escalations[0].Run.ID != "escalated-run" ||
		got.Escalations[0].Cause == nil ||
		got.Escalations[0].Cause.Selector.Kind != "gate" ||
		got.Escalations[0].Cause.Selector.Name != "review" ||
		got.Escalations[0].Cause.RepassCount != 3 {
		t.Fatalf("escalations = %+v", got.Escalations)
	}
}

func TestEscalationsShowArtifactTimelineAndCurrentState(t *testing.T) {
	root := t.TempDir()
	createEscalationInspectionRun(t, root, "escalated-timeline")

	code, stdout, stderr := runArgs(t, "escalations", "show", "escalated-time", root)
	if code != 0 {
		t.Fatalf("escalations show: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"selector: gate/review",
		"repasses: 3",
		"reason: repass budget exhausted",
		"stage=query-backlog attempt=1 class=initial status=success",
		"stage=implement attempt=1 class=initial status=success",
		"query.json digest=",
		"result.json digest=",
		"current state:\n  phase: escalated",
		"verdict.json digest=",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("escalations show stdout missing %q: %q", want, stdout)
		}
	}

	code, stdout, stderr = runArgs(t, "escalations", "show", "--json", "escalated-timeline", root)
	if code != 0 {
		t.Fatalf("escalations show --json: code = %d, stderr = %q", code, stderr)
	}
	var got escalationInspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("escalations show --json produced invalid JSON: %v\n%s", err, stdout)
	}
	if got.Run.ID != "escalated-timeline" ||
		got.Cause == nil ||
		got.Cause.RepassCount != 3 ||
		got.Cause.TerminalReason != "repass budget exhausted" ||
		got.CurrentState.Phase != journal.PhaseEscalated {
		t.Fatalf("inspection summary = %+v", got)
	}
	if len(got.Timeline) != 2 {
		t.Fatalf("timeline length = %d, want 2: %+v", len(got.Timeline), got.Timeline)
	}
	query, implement := got.Timeline[0], got.Timeline[1]
	if query.Stage != "query-backlog" ||
		len(query.ArtifactsBefore) != 0 ||
		artifactNames(query.ArtifactsAfter) != "query.json" {
		t.Fatalf("query timeline = %+v", query)
	}
	if implement.Stage != "implement" ||
		artifactNames(implement.ArtifactsBefore) != "query.json" ||
		artifactNames(implement.ArtifactsAfter) != "query.json,result.json" {
		t.Fatalf("implement timeline = %+v", implement)
	}
	if names := artifactNames(got.CurrentState.Artifacts); names != "query.json,result.json,verdict.json" {
		t.Fatalf("current artifacts = %q, want query.json,result.json,verdict.json", names)
	}
}

func TestEscalationsShowRejectsAmbiguousRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	first := "escalated-ambiguous-one"
	second := "escalated-ambiguous-two"
	createEscalationInspectionRun(t, root, first)
	createEscalationInspectionRun(t, root, second)

	code, stdout, stderr := runArgs(t, "escalations", "show", "escalated-ambiguous", root)
	if code != 2 {
		t.Fatalf("escalations show: code = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("escalations show stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"ambiguous prefix", first, second} {
		if !strings.Contains(stderr, want) {
			t.Errorf("escalations show stderr missing %q: %q", want, stderr)
		}
	}
}

func createEscalationInspectionRun(t *testing.T, root, runID string) {
	t.Helper()
	startedAt := time.Date(2026, time.July, 20, 8, 0, 0, 0, time.UTC)
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 3,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "464"},
		StartedAt:       startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	appendEvent(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "query-backlog",
		Attempt: 1,
	})
	queryRef, err := run.RecordStageArtifact(
		"query-backlog",
		1,
		"",
		"query.json",
		[]byte(`{"item":464}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	queryRef.MediaType = "application/json"
	appendEvent(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "query-backlog",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{queryRef},
	})

	appendEvent(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "implement",
		Attempt: 1,
	})
	resultRef, err := run.RecordStageArtifact(
		"implement",
		1,
		"",
		"result.json",
		[]byte(`{"status":"success"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	resultRef.MediaType = "application/json"
	appendEvent(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "implement",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{resultRef},
	})

	verdictRef, err := run.RecordArtifact("verdict.json", []byte(`{"decision":"needs-changes"}`))
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: string(apiv1.VerdictNeedsChanges),
		Target:  workflow.TargetEscalate,
		Name:    "verdict.json",
		Ref:     &verdictRef,
		Runner: map[string]any{
			"escalated":     true,
			"repassAttempt": 3,
		},
	})
	appendEvent(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseEscalated),
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func artifactNames(artifacts []readservice.ArtifactMetadata) string {
	names := make([]string, len(artifacts))
	for i, artifact := range artifacts {
		names[i] = artifact.Name
	}
	return strings.Join(names, ",")
}

func TestEscalationsShowUnfinishedStageArtifactsInTimeline(t *testing.T) {
	root := t.TempDir()
	startedAt := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           "escalated-open-stage",
		Workflow:        "implementation",
		WorkflowVersion: 3,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "464"},
		StartedAt:       startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	// query-backlog runs to completion.
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "query-backlog", Attempt: 1})
	queryRef, err := run.RecordStageArtifact("query-backlog", 1, "", "query.json", []byte(`{"item":464}`))
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "query-backlog",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{queryRef},
	})

	// implement starts and produces an artifact, but the run escalates before it
	// emits StageFinished — the stage is still open at escalation time.
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	resultRef, err := run.RecordStageArtifact("implement", 1, "", "result.json", []byte(`{"status":"in-progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	verdictRef, err := run.RecordArtifact("verdict.json", []byte(`{"decision":"needs-changes"}`))
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(journal.Event{
		Type:      journal.EventGateEvaluated,
		Gate:      "review",
		Verdict:   string(apiv1.VerdictNeedsChanges),
		Target:    workflow.TargetEscalate,
		Name:      "verdict.json",
		Ref:       &verdictRef,
		Artifacts: []journal.Ref{resultRef},
		Runner: map[string]any{
			"escalated":     true,
			"repassAttempt": 3,
		},
	})
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "escalations", "show", "--json", "escalated-open-stage", root)
	if code != 0 {
		t.Fatalf("escalations show --json: code = %d, stderr = %q", code, stderr)
	}
	var got escalationInspection
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("escalations show --json produced invalid JSON: %v\n%s", err, stdout)
	}

	var implement *escalationArtifactStep
	for i := range got.Timeline {
		if got.Timeline[i].Stage == "implement" {
			implement = &got.Timeline[i]
		}
	}
	if implement == nil {
		t.Fatalf("timeline missing implement stage: %+v", got.Timeline)
	}
	if implement.Status != "running" {
		t.Fatalf("implement status = %q, want running (unfinished stage)", implement.Status)
	}
	// The fix: an unfinished stage's ArtifactsAfter is snapshotted at escalation
	// instead of being left empty, so result.json is attributed to the timeline
	// and not only to the run's current state.
	if names := artifactNames(implement.ArtifactsAfter); !strings.Contains(names, "result.json") {
		t.Fatalf("implement ArtifactsAfter = %q, want it to include result.json (unfinished-stage artifact)", names)
	}
}
