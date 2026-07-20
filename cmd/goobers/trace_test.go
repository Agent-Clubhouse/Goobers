package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
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

func TestTraceJSONPreservesAttemptClassCompatibility(t *testing.T) {
	root := t.TempDir()
	const runID = "attempt-projection"
	run := newTraceTestRun(t, root, runID)
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptPolicy},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptPolicy, Status: string(apiv1.ResultSuccess)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
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
	var classes []string
	for _, event := range got.Events {
		if event.Type == journal.EventStageStarted {
			classes = append(classes, string(event.AttemptClass))
		}
	}
	if strings.Join(classes, ",") != ",policy" {
		t.Fatalf("attempt classes = %v, want empty initial class then policy", classes)
	}
}

func TestTraceRendersPersistedCIFailureEvidence(t *testing.T) {
	root := t.TempDir()
	const runID = "ci-failure-evidence"
	run := newTraceTestRun(t, root, runID)
	if _, err := run.RecordArtifact("unrelated/"+executor.CIChecksArtifactName, []byte("not CI evidence")); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "ci-poll", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	artifactData, err := json.Marshal(executor.CIChecksArtifact{
		Checks: []providers.CheckDetail{
			{Name: "unit-tests", State: providers.CheckStateFailing, URL: "https://ci.example/unit", Summary: "panic in TestWidget\nfull stack"},
			{Name: "integration", State: providers.CheckStatePending, URL: "https://ci.example/integration", Summary: "still running"},
			{Name: "lint\ninjected", State: providers.CheckStateFailing, URL: "https://ci.example/lint\nignored", Summary: "format mismatch\r\nsecond line"},
		},
		Metadata: executor.CIChecksArtifactMetadata{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := run.RecordArtifact(executor.CIChecksArtifactName, artifactData)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "ci-poll", Attempt: 1,
		Status:    string(apiv1.ResultSuccess),
		Outputs:   map[string]any{executor.OutputCIStatus: string(providers.CheckStateFailing)},
		Artifacts: []journal.Ref{ref},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"CI failed checks:\n",
		`check="unit-tests" summary="panic in TestWidget" url="https://ci.example/unit"`,
		`check="lint\ninjected" summary="format mismatch" url="https://ci.example/lint\nignored"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace output missing %q:\n%s", want, stdout)
		}
	}
	for _, unwanted := range []string{"full stack", "second line", `check="integration"`} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("trace output contains %q:\n%s", unwanted, stdout)
		}
	}
}

func TestTraceJSONPreservesIdentityRefsAndMissingState(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "json-compatibility"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "512"},
	}, map[string][]byte{"item": []byte(`{"number":512}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "copilot-cli.transcript", []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifact("result.json", []byte(`{"status":"success"}`)); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(layout.RunsDir(), runID, "state.json")); err != nil {
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
	if got.State != nil {
		t.Fatalf("state = %+v, want omitted for missing checkpoint", got.State)
	}
	if len(got.Identity.Inputs) != 1 ||
		got.Identity.Inputs[0].Name != "item" ||
		!strings.HasPrefix(got.Identity.Inputs[0].Ref.Path, "inputs/") ||
		got.Identity.Inputs[0].Ref.Digest == "" {
		t.Fatalf("identity inputs = %+v", got.Identity.Inputs)
	}
	var artifactRef, spanRef *journal.Ref
	for i := range got.Events {
		switch got.Events[i].Type {
		case journal.EventArtifactRecorded:
			artifactRef = got.Events[i].Ref
		case journal.EventSpanRecorded:
			spanRef = got.Events[i].Ref
		}
	}
	if artifactRef == nil || !strings.HasPrefix(artifactRef.Path, "artifacts/") || artifactRef.Digest == "" {
		t.Fatalf("artifact ref = %+v", artifactRef)
	}
	if spanRef == nil || !strings.HasPrefix(spanRef.Path, "spans/") || spanRef.Digest == "" {
		t.Fatalf("span ref = %+v", spanRef)
	}
}

func TestTraceListsRecordedTranscripts(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-list"
	run := newTraceTestRun(t, root, runID)
	if _, err := run.RecordSpan(runID+":query-backlog", "copilot-cli.transcript", []byte("selected issue 477\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan(runID+":query-backlog", "copilot-cli.tool-events", []byte("internal tool event")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan(runID+":implement", "copilot-cli.transcript", []byte("implemented trace views")); err != nil {
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
	if _, err := run.RecordSpan(runID+":query-backlog", "copilot-cli.transcript", []byte("query transcript")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan(runID+":implement", "copilot-cli.transcript", []byte("implementation transcript")); err != nil {
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

func TestTraceFollowStreamsLiveEventsOnceInOrder(t *testing.T) {
	root := t.TempDir()
	const runID = "follow-live-events-run"
	run := newTraceTestRun(t, root, runID)
	t.Cleanup(func() { _ = run.Close() })

	stdout := newTraceFollowBuffer()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runTraceWithFollowContext(
			context.Background(),
			[]string{"--follow", "follow-live", root},
			stdout,
			&stderr,
		)
	}()
	stdout.waitForWrite(t)

	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if code := waitForTraceFollow(t, result); code != 0 {
		t.Fatalf("trace --follow: code = %d, stderr = %q", code, stderr.String())
	}
	want := strings.Join([]string{
		"[1] run.started status=running",
		"[2] stage.started stage=implement attempt=1",
		"[3] stage.finished stage=implement attempt=1 status=success",
		"[4] run.finished status=completed",
		"",
	}, "\n")
	if got := stdout.String(); got != want {
		t.Fatalf("trace --follow stdout = %q, want %q", got, want)
	}
}

func TestTraceJSONFollowEmitsExistingEventShapeAsJSONLines(t *testing.T) {
	root := t.TempDir()
	const runID = "follow-json-lines-run"
	run := newTraceTestRun(t, root, runID)
	t.Cleanup(func() { _ = run.Close() })

	stdout := newTraceFollowBuffer()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runTraceWithFollowContext(
			context.Background(),
			[]string{"--json", "--follow", runID, root},
			stdout,
			&stderr,
		)
	}()
	stdout.waitForWrite(t)

	for _, event := range []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "local-ci"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if code := waitForTraceFollow(t, result); code != 0 {
		t.Fatalf("trace --json --follow: code = %d, stderr = %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("JSON Lines count = %d, want 3: %q", len(lines), stdout.String())
	}
	wantTypes := []journal.EventType{
		journal.EventRunStarted,
		journal.EventGateEvaluated,
		journal.EventRunFinished,
	}
	for i, line := range lines {
		var event traceJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %d is not valid JSON: %v: %q", i+1, err, line)
		}
		if event.Type != wantTypes[i] || event.Seq != uint64(i+1) {
			t.Fatalf("line %d event = type %q seq %d, want type %q seq %d", i+1, event.Type, event.Seq, wantTypes[i], i+1)
		}
		var shape map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &shape); err != nil {
			t.Fatal(err)
		}
		if _, ok := shape["events"]; ok {
			t.Fatalf("line %d unexpectedly wraps events: %q", i+1, line)
		}
		if _, ok := shape["identity"]; ok {
			t.Fatalf("line %d unexpectedly contains a trace summary: %q", i+1, line)
		}
	}
}

func TestTraceFollowTerminalRunUsesOrdinaryTraceOutput(t *testing.T) {
	root := t.TempDir()
	const runID = "follow-terminal-run"
	run := newTraceTestRun(t, root, runID)
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runTraceWithFollowContext(
		context.Background(),
		[]string{"--json", "--follow", runID, root},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("terminal trace --follow: code = %d, stderr = %q", code, stderr.String())
	}
	var got traceJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("terminal trace --follow did not use ordinary JSON output: %v: %q", err, stdout.String())
	}
	if got.Identity.RunID != runID || got.Phase != journal.PhaseCompleted || len(got.Events) != 2 {
		t.Fatalf("terminal trace --follow = identity %q phase %q events %d", got.Identity.RunID, got.Phase, len(got.Events))
	}
}

func TestTraceFollowRefreshesRunThatFinishesBeforeInitialEventRead(t *testing.T) {
	root := t.TempDir()
	const runID = "follow-startup-race"
	run := newTraceTestRun(t, root, runID)

	var raceReader *traceFinishOnEventsReader
	factory := func(layout instance.Layout) (readservice.OfflineRuns, error) {
		reads, err := readservice.NewOfflineRuns(layout)
		if err != nil {
			return nil, err
		}
		raceReader = &traceFinishOnEventsReader{
			OfflineRuns: reads,
			run:         run,
		}
		return raceReader, nil
	}

	var stdout, stderr bytes.Buffer
	code := runTraceWithFollowContextAndFactory(
		context.Background(),
		[]string{"--json", "--follow", runID, root},
		&stdout,
		&stderr,
		factory,
	)
	if code != 0 {
		t.Fatalf("terminal trace --follow: code = %d, stderr = %q", code, stderr.String())
	}
	var got traceJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("terminal trace --follow did not use ordinary JSON output: %v: %q", err, stdout.String())
	}
	if got.Phase != journal.PhaseCompleted || got.State == nil || got.State.Phase != journal.PhaseCompleted {
		t.Fatalf("terminal trace --follow phase = %q, state = %#v", got.Phase, got.State)
	}
	if raceReader.getRunCalls != 2 {
		t.Fatalf("GetRun calls = %d, want initial read and terminal refresh", raceReader.getRunCalls)
	}
}

type traceFinishOnEventsReader struct {
	readservice.OfflineRuns
	run         *journal.Run
	getRunCalls int
	finishOnce  sync.Once
	finishErr   error
}

func (r *traceFinishOnEventsReader) GetRun(ctx context.Context, runID string) (readservice.RunDetail, error) {
	r.getRunCalls++
	return r.OfflineRuns.GetRun(ctx, runID)
}

func (r *traceFinishOnEventsReader) RunEvents(ctx context.Context, runID string) (readservice.EventList, error) {
	r.finishOnce.Do(func() {
		r.finishErr = r.run.Append(journal.Event{
			Type:   journal.EventRunFinished,
			Status: string(journal.PhaseCompleted),
		})
		if r.finishErr == nil {
			r.finishErr = r.run.Close()
		}
	})
	if r.finishErr != nil {
		return readservice.EventList{}, r.finishErr
	}
	return r.OfflineRuns.RunEvents(ctx, runID)
}

func TestTraceFollowCancellationSkipsTornRecordAndExitsInterrupted(t *testing.T) {
	root := t.TempDir()
	const runID = "follow-cancel-run"
	run := newTraceTestRun(t, root, runID)
	t.Cleanup(func() { _ = run.Close() })

	eventsPath := filepath.Join(instance.NewLayout(root).RunsDir(), runID, "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schema":"goobers.dev/journal/event/v1","seq":2`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout := newTraceFollowBuffer()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runTraceWithFollowContext(
			ctx,
			[]string{"--json", "--follow", runID, root},
			stdout,
			&stderr,
		)
	}()
	stdout.waitForWrite(t)
	cancel()

	if code := waitForTraceFollow(t, result); code != traceInterruptedExitCode {
		t.Fatalf("cancelled trace --follow: code = %d, want %d; stderr = %q", code, traceInterruptedExitCode, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("cancelled JSON Lines = %q, want one complete event", stdout.String())
	}
	var event traceJSONEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("cancelled trace emitted a partial record: %v: %q", err, stdout.String())
	}
	if event.Type != journal.EventRunStarted || event.Seq != 1 {
		t.Fatalf("cancelled trace event = type %q seq %d", event.Type, event.Seq)
	}
	if stderr.Len() != 0 {
		t.Fatalf("cancelled trace stderr = %q, want empty", stderr.String())
	}
}

func TestTraceFollowRejectsTranscriptFlags(t *testing.T) {
	for _, transcriptFlag := range []string{"--transcripts", "--transcript=implement"} {
		t.Run(transcriptFlag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runTraceWithFollowContext(
				context.Background(),
				[]string{"--follow", transcriptFlag, "run-id"},
				&stdout,
				&stderr,
			)
			if code != 2 || stdout.Len() != 0 ||
				!strings.Contains(stderr.String(), "--follow cannot be used with --transcripts or --transcript") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

type traceFollowBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	once  sync.Once
	wrote chan struct{}
}

func newTraceFollowBuffer() *traceFollowBuffer {
	return &traceFollowBuffer{wrote: make(chan struct{})}
}

func (b *traceFollowBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	if n > 0 {
		b.once.Do(func() { close(b.wrote) })
	}
	return n, err
}

func (b *traceFollowBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *traceFollowBuffer) waitForWrite(t *testing.T) {
	t.Helper()
	select {
	case <-b.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for trace --follow output")
	}
}

func waitForTraceFollow(t *testing.T, result <-chan int) int {
	t.Helper()
	select {
	case code := <-result:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for trace --follow to exit")
		return -1
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

func TestTracePrefixIgnoresCorruptUnrelatedRun(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	const runID = "dd57a3c2f0d27ea99ca7fa84db6ecab4"
	createTraceRun(t, root, runID)
	corruptDir := filepath.Join(layout.RunsDir(), "unrelated-corrupt-run")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "run.yaml"), []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", "dd57a3c2", root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "run:      "+runID+"\n") {
		t.Fatalf("trace stdout missing resolved run id: %q", stdout)
	}
}

func TestTracePreservesUnknownEventSchemas(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "future-events"
	createTraceRun(t, root, runID)

	eventsPath := filepath.Join(l.RunsDir(), runID, "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	unknown := `{"schema":"goobers.dev/journal/event/v99","seq":2,"type":"future.event","branch":4,"time":"2026-07-18T12:00:00Z","future":{"answer":42}}`
	if _, err := file.WriteString(unknown + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
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
	if len(got.Events) != 2 ||
		got.Events[1].KnownSchema == nil ||
		*got.Events[1].KnownSchema ||
		got.Events[1].Seq != 2 ||
		got.Events[1].Branch != 4 ||
		!strings.Contains(string(got.Events[1].Raw), `"answer":42`) {
		t.Fatalf("future event = %+v", got.Events)
	}
}

func TestTraceRejectsSymlinkedRunDirectory(t *testing.T) {
	root := t.TempDir()
	outsideRoot := t.TempDir()
	const runID = "symlinked-run"
	createTraceRun(t, outsideRoot, runID)

	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(instance.NewLayout(outsideRoot).RunsDir(), runID),
		filepath.Join(layout.RunsDir(), runID),
	); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "trace", runID, root)
	if code != 1 {
		t.Fatalf("trace: code = %d, want 1; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("trace stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `no run "symlinked-run" found`) {
		t.Fatalf("trace stderr = %q", stderr)
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
			name: "stage heartbeat",
			event: journal.Event{
				Seq: 5, Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 2,
				AttemptClass: journal.AttemptPolicy,
			},
			want: "[5] stage.heartbeat stage=implement attempt=2 class=policy",
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
			name: "stage finished with outputs",
			event: journal.Event{
				Seq: 5, Type: journal.EventStageFinished, Stage: "check-todos", Attempt: 1,
				AttemptClass: journal.AttemptPolicy, Status: "success",
				Outputs: map[string]any{"todoCount": float64(2)},
			},
			want: `[5] stage.finished stage=check-todos attempt=1 class=policy status=success outputs={"todoCount":2}`,
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
			if !strings.Contains(stdout.String(), "\nlast activity: ") {
				t.Fatalf("trace stdout missing last activity: %q", stdout.String())
			}
			if !strings.Contains(stdout.String(), "\n"+tt.want+"\nevents:") {
				t.Fatalf("trace stdout missing repass header %q after phase: %q", tt.want, stdout.String())
			}
		})
	}
}
