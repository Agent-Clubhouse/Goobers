package main

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestTraceShowsEscalationSummary(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const (
		runID  = "escalated-run"
		reason = "The implementation still drops the reviewer context."
	)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("implement")
	for _, ev := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
	} {
		if err := jr.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	verdictData, err := json.Marshal(apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := jr.RecordArtifact("verdict/review-4.json", verdictData)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: string(apiv1.VerdictNeedsChanges),
		Target:  workflow.TargetEscalate,
		Name:    "verdict/review-4.json",
		Ref:     &ref,
		Runner:  map[string]any{"repassAttempt": 4, "escalated": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	wantSummary := "⚠ ESCALATED\n" +
		"  stage: implement\n" +
		"  gate: review\n" +
		"  repass count: 4\n" +
		"  last needs-changes reason: " + reason + "\n\n"
	if !strings.HasPrefix(stdout, wantSummary) {
		t.Fatalf("trace stdout = %q, want prefix %q", stdout, wantSummary)
	}
	if !strings.Contains(stdout, "events:") {
		t.Fatalf("trace stdout missing raw events after summary: %q", stdout)
	}
}

func TestTraceOmitsEscalationSummaryForCompletedRun(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "completed-run"
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "ESCALATED") {
		t.Fatalf("completed trace unexpectedly contains escalation summary: %q", stdout)
	}
}
