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

func TestFormatEvent(t *testing.T) {
	tests := []struct {
		name  string
		event journal.Event
		want  string
	}{
		{
			name:  "run started",
			event: journal.Event{Seq: 1, Type: journal.EventRunStarted},
			want:  "[1] run.started",
		},
		{
			name:  "run finished",
			event: journal.Event{Seq: 2, Type: journal.EventRunFinished, Status: "completed"},
			want:  "[2] run.finished status=completed",
		},
		{
			name: "stage started initial attempt",
			event: journal.Event{
				Seq: 3, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1,
			},
			want: "[3] stage.started stage=implement attempt=1",
		},
		{
			name: "stage started retry",
			event: journal.Event{
				Seq: 4, Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
				AttemptClass: journal.AttemptInfra,
			},
			want: "[4] stage.started stage=implement attempt=2 class=infra",
		},
		{
			name: "stage finished",
			event: journal.Event{
				Seq: 5, Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
				AttemptClass: journal.AttemptPolicy, Status: "success",
			},
			want: "[5] stage.finished stage=implement attempt=2 class=policy status=success",
		},
		{
			name: "gate evaluated",
			event: journal.Event{
				Seq: 6, Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci",
			},
			want: "[6] gate.evaluated gate=review verdict=pass target=local-ci",
		},
		{
			name: "artifact recorded",
			event: journal.Event{
				Seq: 7, Type: journal.EventArtifactRecorded, Name: "report",
				Ref: &journal.Ref{Digest: "sha256:abc", Size: 42},
			},
			want: "[7] artifact.recorded name=report digest=sha256:abc size=42",
		},
		{
			name:  "artifact recorded without ref",
			event: journal.Event{Seq: 8, Type: journal.EventArtifactRecorded, Name: "report"},
			want:  "[8] artifact.recorded name=report",
		},
		{
			name: "input snapshot",
			event: journal.Event{
				Seq: 9, Type: journal.EventInputSnapshot, Name: "issue",
				Ref: &journal.Ref{Digest: "sha256:def", Size: 84},
			},
			want: "[9] input.snapshot name=issue digest=sha256:def size=84",
		},
		{
			name: "external ref touched",
			event: journal.Event{
				Seq: 10, Type: journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{
					Provider: "github", Kind: "issue", ID: "333", URL: "https://example.test/issues/333",
				},
			},
			want: "[10] ref.touched provider=github kind=issue id=333 url=https://example.test/issues/333",
		},
		{
			name:  "external ref touched without details",
			event: journal.Event{Seq: 11, Type: journal.EventRefTouched},
			want:  "[11] ref.touched",
		},
		{
			name: "error",
			event: journal.Event{
				Seq: 12, Type: journal.EventError,
				Error: &journal.ErrorDetail{Code: "provider_failed", Message: "request failed"},
			},
			want: `[12] error code=provider_failed message="request failed"`,
		},
		{
			name:  "error without details",
			event: journal.Event{Seq: 13, Type: journal.EventError},
			want:  "[13] error",
		},
		{
			name: "redaction",
			event: journal.Event{
				Seq: 14, Type: journal.EventRedaction,
				Redaction: &journal.RedactionInfo{
					Target: "artifacts/secret", OldDigest: "sha256:old", NewDigest: "sha256:new",
				},
			},
			want: "[14] redaction target=artifacts/secret old=sha256:old new=sha256:new",
		},
		{
			name:  "redaction without details",
			event: journal.Event{Seq: 15, Type: journal.EventRedaction},
			want:  "[15] redaction",
		},
		{
			name:  "span recorded",
			event: journal.Event{Seq: 16, Type: journal.EventSpanRecorded},
			want:  "[16] span.recorded",
		},
		{
			name:  "journal repaired",
			event: journal.Event{Seq: 17, Type: journal.EventRepaired},
			want:  "[17] repaired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatEvent(tt.event); got != tt.want {
				t.Errorf("formatEvent() = %q, want %q", got, tt.want)
			}
		})
	}
}
