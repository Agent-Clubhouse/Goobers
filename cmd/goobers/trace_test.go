package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

func TestTraceJSONIncludesFailedRunErrorAndSpans(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "failed-json-run"
	startedAt := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 3,
		WorkflowDigest:  "sha256:fixture",
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "556"},
		StartedAt:       startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type:  journal.EventError,
		Stage: "implement",
		Error: &journal.ErrorDetail{Code: "stage_failed", Message: "implementation failed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	spanDir := filepath.Join(l.RunsDir(), runID, "spans")
	if err := os.MkdirAll(spanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	span := telemetry.SpanRecord{
		Schema:    telemetry.SpanSchema,
		TraceID:   runID,
		SpanID:    "0123456789abcdef",
		Name:      "task/implement",
		Kind:      telemetry.SpanKindTask,
		StartTime: startedAt.Add(time.Second),
		EndTime:   startedAt.Add(2500 * time.Millisecond),
		Status:    "error",
	}
	spanData, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spanDir, "spans.jsonl"), append(spanData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--json", runID, root)
	if code != 0 {
		t.Fatalf("trace --json: code = %d, stderr = %q", code, stderr)
	}
	var got traceJSONResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("trace --json produced invalid JSON: %v\n%s", err, stdout)
	}
	if got.Identity.RunID != runID || got.Identity.Workflow != "implementation" ||
		got.Identity.WorkflowVersion != 3 || got.Identity.WorkflowDigest != "sha256:fixture" ||
		got.Identity.Trigger.Kind != journal.TriggerItem || got.Identity.Trigger.Ref != "556" {
		t.Fatalf("identity = %#v", got.Identity)
	}
	if got.Phase != journal.PhaseFailed || got.State == nil || got.State.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, state = %#v", got.Phase, got.State)
	}
	var terminalError *journal.ErrorDetail
	for _, event := range got.Events {
		if event.Type == journal.EventError {
			terminalError = event.Error
		}
	}
	if terminalError == nil || terminalError.Code != "stage_failed" || terminalError.Message != "implementation failed" {
		t.Fatalf("events missing terminal error: %#v", got.Events)
	}
	if len(got.Spans) != 1 || got.Spans[0].Name != "task/implement" ||
		got.Spans[0].Status != "error" || got.Spans[0].DurationMs != 1500 {
		t.Fatalf("spans = %#v", got.Spans)
	}
}

func TestTraceListsRecordedTranscripts(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-list"
	run := newTraceTestRun(t, root, runID)
	if _, err := run.RecordSpan("query-backlog", "copilot-cli.transcript", []byte("selected issue 477\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("query-backlog", "copilot-cli.tool-events", []byte("internal tool event")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "copilot-cli.transcript", []byte("implemented trace views")); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--transcripts", runID, root)
	if code != 0 {
		t.Fatalf("trace --transcripts: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"transcripts:\n",
		`stage="query-backlog" name="copilot-cli.transcript"`,
		"selected issue 477\n",
		`stage="implement" name="copilot-cli.transcript"`,
		"implemented trace views\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("trace --transcripts stdout missing %q: %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "internal tool event") {
		t.Fatalf("trace --transcripts included a non-transcript span: %q", stdout)
	}
}

func TestTraceSelectsStageTranscript(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-stage"
	run := newTraceTestRun(t, root, runID)
	if _, err := run.RecordSpan("query-backlog", "copilot-cli.transcript", []byte("query transcript")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "copilot-cli.transcript", []byte("implementation transcript")); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--transcript", "implement", runID, root)
	if code != 0 {
		t.Fatalf("trace --transcript: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `stage="implement"`) || !strings.Contains(stdout, "implementation transcript") {
		t.Fatalf("trace --transcript stdout missing selected stage: %q", stdout)
	}
	if strings.Contains(stdout, "query transcript") {
		t.Fatalf("trace --transcript included an unselected stage: %q", stdout)
	}
}

func TestTraceReportsMissingTranscript(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-missing"
	run := newTraceTestRun(t, root, runID)
	if _, err := run.RecordSpan("review", "copilot-cli.transcript", []byte("review transcript")); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--transcript=implement", runID, root)
	if code != 1 {
		t.Fatalf("trace missing transcript: code = %d, want 1; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("trace missing transcript stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `no recorded agent transcript for stage "implement"`) {
		t.Fatalf("trace missing transcript stderr = %q", stderr)
	}
}

func TestTraceReportsUnavailableTranscript(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-unavailable"
	run := newTraceTestRun(t, root, runID)
	ref, err := run.RecordSpan("implement", "copilot-cli.transcript", []byte("implementation transcript"))
	if err != nil {
		t.Fatal(err)
	}
	runDir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runDir, ref.Path)); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "--transcripts", runID, root)
	if code != 2 {
		t.Fatalf("trace unavailable transcript: code = %d, want 2; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("trace unavailable transcript stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `transcript for stage "implement"`) || !strings.Contains(stderr, "is unavailable") {
		t.Fatalf("trace unavailable transcript stderr = %q", stderr)
	}
}

func newTraceTestRun(t *testing.T, root, runID string) *journal.Run {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestTraceResolvesUniqueRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	const runID = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	createTraceRun(t, root, runID)

	code, stdout, stderr := runArgs(t, "trace", "dd57a3c2", root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "run:      "+runID+"\n") {
		t.Fatalf("trace stdout missing resolved run id: %q", stdout)
	}
}

func TestTraceRejectsAmbiguousRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	const (
		first  = "dd57a3c2aaaaaaaaaaaaaaaaaaaaaaaa"
		second = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	)
	createTraceRun(t, root, first)
	createTraceRun(t, root, second)

	code, stdout, stderr := runArgs(t, "trace", "dd57a3c2", root)
	if code != 2 {
		t.Fatalf("trace: code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("trace stdout = %q, want empty", stdout)
	}
	want := `error: ambiguous prefix "dd57a3c2" matches 2 runs: ` + first + ", " + second + "\n"
	if stderr != want {
		t.Fatalf("trace stderr = %q, want %q", stderr, want)
	}
}

func TestTracePrefersExactRunIDOverPrefixMatches(t *testing.T) {
	root := t.TempDir()
	const (
		exact  = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
		longer = exact + "-other"
	)
	createTraceRun(t, root, exact)
	createTraceRun(t, root, longer)

	code, stdout, stderr := runArgs(t, "trace", exact, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "run:      "+exact+"\n") {
		t.Fatalf("trace stdout missing exact run id: %q", stdout)
	}
}

func createTraceRun(t *testing.T, root, runID string) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create trace run %q: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close trace run %q: %v", runID, err)
	}
}

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

	jsonCode, jsonStdout, jsonStderr := runArgs(t, "trace", "--json", runID, root)
	if jsonCode != 0 {
		t.Fatalf("trace --json: code = %d, stderr = %q", jsonCode, jsonStderr)
	}
	var got traceJSONResult
	if err := json.Unmarshal([]byte(jsonStdout), &got); err != nil {
		t.Fatalf("trace --json produced invalid JSON: %v\n%s", err, jsonStdout)
	}
	if got.Escalation == nil || got.Escalation.Stage != "implement" ||
		got.Escalation.Gate != "review" || got.Escalation.RepassCount != 4 ||
		got.Escalation.LastNeedsChangesReason != reason {
		t.Fatalf("escalation = %#v", got.Escalation)
	}
	if got.Spans == nil {
		t.Fatalf("spans = nil, want an empty JSON array")
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

func TestTraceResolvesUniqueRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	const runID = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	createTraceRun(t, root, runID)

	code, stdout, stderr := runArgs(t, "trace", "dd57a3c2", root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "run:      "+runID+"\n") {
		t.Fatalf("trace stdout missing resolved run id: %q", stdout)
	}
}

func TestTraceRejectsAmbiguousRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	const (
		first  = "dd57a3c2aaaaaaaaaaaaaaaaaaaaaaaa"
		second = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	)
	createTraceRun(t, root, first)
	createTraceRun(t, root, second)

	code, stdout, stderr := runArgs(t, "trace", "dd57a3c2", root)
	if code != 2 {
		t.Fatalf("trace: code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("trace stdout = %q, want empty", stdout)
	}
	want := `error: ambiguous prefix "dd57a3c2" matches 2 runs: ` + first + ", " + second + "\n"
	if stderr != want {
		t.Fatalf("trace stderr = %q, want %q", stderr, want)
	}
}

func TestTracePrefersExactRunIDOverPrefixMatches(t *testing.T) {
	root := t.TempDir()
	const (
		exact  = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
		longer = exact + "-other"
	)
	createTraceRun(t, root, exact)
	createTraceRun(t, root, longer)

	code, stdout, stderr := runArgs(t, "trace", exact, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "run:      "+exact+"\n") {
		t.Fatalf("trace stdout missing exact run id: %q", stdout)
	}
}

func createTraceRun(t *testing.T, root, runID string) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create trace run %q: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close trace run %q: %v", runID, err)
	}
}

// TestTraceRepassCountsAreConsistentAcrossMultipleGates is #354: the header
// `repasses:` line and the ESCALATED block's `repass count:` used to be two
// independently-hardcoded computations that could silently disagree. They
// now share one repassCount helper (gate == "" for the whole-run header,
// gate == <name> for the escalation block's per-gate streak) — this fixture
// deliberately has two DIFFERENT gates repass so the two numbers come out
// genuinely different (3 vs 2), proving each is correctly, independently
// derived from the shared rule rather than the fix trivially forcing them
// equal.
func TestTraceRepassCountsAreConsistentAcrossMultipleGates(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "multi-gate-escalated-run"

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []journal.Event{
		// local-gate repasses once — contributes to the whole-run header
		// total but is not part of review's own streak.
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement"},
		// review repasses, passes once (resetting its streak), then
		// repasses twice more, the second time escalating.
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictPass), Target: "local-ci"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictNeedsChanges), Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	} {
		if err := jr.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	// Whole-run header: 3 events targeted "implement" (local-gate once,
	// review twice — review's pass at position 3 isn't implement-targeted
	// so it doesn't count, and the final escalating verdict targets
	// "escalate" not "implement" so it doesn't count either).
	if !strings.Contains(stdout, "\nrepasses: 3\n") {
		t.Fatalf("trace stdout missing whole-run header repasses=3: %q", stdout)
	}
	// Escalation block: review's own streak since its last pass is 2 (the
	// pre-pass needs-changes doesn't count; the two after the reset do).
	if !strings.Contains(stdout, "  repass count: 2\n") {
		t.Fatalf("trace stdout missing escalation-block repass count=2: %q", stdout)
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

func TestTraceShowsRepassCount(t *testing.T) {
	tests := []struct {
		name   string
		events []journal.Event
		want   string
	}{
		{
			name: "repasses",
			events: []journal.Event{
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: "implement", Runner: map[string]any{"repassAttempt": 1}},
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci", Runner: map[string]any{"repassAttempt": 0}},
				{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "fail", Target: "implement", Runner: map[string]any{"repassAttempt": 1}},
			},
			want: "repasses: 2\n",
		},
		{
			name: "single pass",
			events: []journal.Event{
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci", Runner: map[string]any{"repassAttempt": 0}},
			},
			want: "repasses: 0\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runID := "trace-" + strings.ReplaceAll(tt.name, " ", "-")
			run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
				RunID:           runID,
				Workflow:        "implementation",
				WorkflowVersion: 1,
				Gaggle:          "goobers",
				Trigger:         journal.Trigger{Kind: journal.TriggerManual},
			}, nil)
			if err != nil {
				t.Fatalf("create fixture run: %v", err)
			}
			for _, ev := range tt.events {
				if err := run.Append(ev); err != nil {
					t.Fatalf("append fixture event: %v", err)
				}
			}
			if err := run.Close(); err != nil {
				t.Fatalf("close fixture run: %v", err)
			}

			var stdout, stderr bytes.Buffer
			if code := runTrace([]string{runID, root}, &stdout, &stderr); code != 0 {
				t.Fatalf("trace code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "phase:    running (machineState=\"\", lastSeq=") {
				t.Fatalf("trace stdout missing phase header: %q", stdout.String())
			}
			if !strings.Contains(stdout.String(), "\n"+tt.want+"\nevents:") {
				t.Fatalf("trace stdout missing repass header %q after phase: %q", tt.want, stdout.String())
			}
		})
	}
}
