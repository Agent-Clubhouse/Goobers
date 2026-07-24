package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// writeFixtureRunWithError hand-constructs a run journal with a recorded
// stage error, exercising `telemetry stats`/`errors`' rollup ingestion
// directly rather than through a real `goobers run` dispatch — issue #23
// rewired `run` onto the real runner (see run.go/daemon_test.go), so a
// generic injected failure like this is no longer something `goobers run`
// itself produces on demand; telemetry's own ingestion is what these tests
// care about; internal/runner's and internal/telemetry/rollup's own test
// suites cover real dispatch/ingestion behavior respectively.
func writeFixtureRunWithError(t *testing.T, root string) {
	t.Helper()
	writeFixtureRunWithErrorForGaggle(t, instance.NewLayout(root), "fixture-run-1", "example")
}

func writeFixtureRunWithErrorForGaggle(t *testing.T, l instance.Layout, runID, gaggle string) {
	t.Helper()
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create fixture run: %v", err)
	}
	defer func() { _ = jr.Close() }()

	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventError, Stage: "implement", Attempt: 1,
		Error: &journal.ErrorDetail{Code: "fixture_error", Message: "fixture-injected failure"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure),
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}

	span := telemetry.SpanRecord{
		Schema:     telemetry.SpanSchema,
		TraceID:    runID,
		SpanID:     "0123456789abcdef",
		Name:       "task/implement",
		Kind:       telemetry.SpanKindTask,
		StartTime:  time.Now().UTC().Add(-time.Minute),
		EndTime:    time.Now().UTC().Add(time.Minute),
		Status:     "error",
		Attributes: map[string]string{telemetry.AttrStage: "implement", telemetry.AttrAttemptNumber: "1"},
		Events: []telemetry.SpanEventRecord{{
			Name: telemetry.GenAIModelUsageEventName,
			Attributes: map[string]string{
				telemetry.AttrGenAIResponseModel:     "gpt-5.4",
				telemetry.AttrGenAIUsageInputTokens:  "120",
				telemetry.AttrGenAIUsageOutputTokens: "30",
				telemetry.AttrCopilotPremiumRequests: "1",
				telemetry.AttrUsageCostUSD:           "0.25",
			},
		}},
	}
	spanData, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	spanDir := filepath.Join(l.RunsDir(), runID, "spans")
	if err := os.MkdirAll(spanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spanDir, "spans.jsonl"), append(spanData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTelemetryStatsAfterRun hand-writes its fixture run directly to disk
// (writeFixtureRunWithError), bypassing `goobers run`/`up` entirely — so
// none of the incremental-ingest hooks issue #127 wires into those commands
// ever fire for it. --rebuild is the explicit, documented way to pick up a
// run journaled out-of-band, exactly the case this test exercises.
func TestTelemetryStatsAfterRun(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "stats", "--rebuild", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "WORKFLOW STATS") ||
		!strings.Contains(stdout, "GAGGLE") ||
		!strings.Contains(stdout, "default-implement") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTelemetryErrorsAfterRun(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "errors", "--rebuild", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "fixture_error") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTelemetryStatsJSON(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "stats", "--json", "--rebuild", root)
	if code != 0 {
		t.Fatalf("telemetry stats --json: code = %d, stderr = %q", code, stderr)
	}
	var got rollup.StatsResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("telemetry stats --json produced invalid JSON: %v\n%s", err, stdout)
	}
	if len(got.Runs) != 1 || got.Runs[0].Workflow != "default-implement" ||
		got.Runs[0].TotalRuns != 1 || got.Runs[0].FailedRuns != 1 {
		t.Fatalf("run stats = %#v", got.Runs)
	}
	if len(got.Gaggles) != 1 || got.Gaggles[0].Gaggle != "example" ||
		got.Gaggles[0].TotalRuns != 1 || got.Gaggles[0].FailedRuns != 1 {
		t.Fatalf("gaggle stats = %#v", got.Gaggles)
	}
	if len(got.Stages) != 1 || got.Stages[0].Stage != "implement" ||
		got.Stages[0].TotalAttempts != 1 || got.Stages[0].FailedAttempts != 1 {
		t.Fatalf("stage stats = %#v", got.Stages)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "gpt-5.4" ||
		got.Models[0].UsageSamples != 1 ||
		got.Models[0].InputTokenSamples != 1 || got.Models[0].InputTokens != 120 ||
		got.Models[0].OutputTokenSamples != 1 || got.Models[0].OutputTokens != 30 ||
		got.Models[0].PremiumRequestSamples != 1 || got.Models[0].CopilotPremiumRequests != 1 ||
		got.Models[0].CostSamples != 1 || got.Models[0].CostUSD != 0.25 {
		t.Fatalf("model stats = %#v", got.Models)
	}

	var document struct {
		Gaggles []json.RawMessage `json:"gaggles"`
		Runs    []json.RawMessage `json:"runs"`
		Stages  []json.RawMessage `json:"stages"`
		Models  []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatal(err)
	}
	assertJSONObjectKeys(t, document.Gaggles[0],
		"gaggle", "totalRuns", "completedRuns", "failedRuns", "otherRuns",
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
	)
	assertJSONObjectKeys(t, document.Runs[0],
		"gaggle", "workflow", "totalRuns", "completedRuns", "failedRuns", "otherRuns",
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
	)
	assertJSONObjectKeys(t, document.Stages[0],
		"gaggle", "workflow", "stage", "totalAttempts", "succeededAttempts", "failedAttempts",
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
		"durationSamples", "p50DurationMs", "p95DurationMs",
		"tokenSamples", "costSamples", "retryWasteAttempts",
	)
	assertJSONObjectKeys(t, document.Models[0],
		"model", "usageSamples",
		"inputTokenSamples", "inputTokens",
		"outputTokenSamples", "outputTokens",
		"premiumRequestSamples", "copilotPremiumRequests",
		"costSamples", "costUSD",
	)
}

func TestTelemetryStatsFiltersAndGroupsAgentProvenance(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	db, err := rollup.Open(l.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", l.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	started := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for i, fixture := range []struct {
		runID, model, version string
	}{
		{"agent-run-1", "gpt-5.6-sol", "copilot version 1.2.3"},
		{"agent-run-2", "claude-sonnet-5", "copilot version 1.2.3"},
	} {
		if _, err := raw.Exec(`
			INSERT INTO runs (
				run_id, workflow, workflow_version, gaggle, status, started_at, finished_at, duration_ms
			) VALUES (?, 'implement', 1, 'example', 'completed', ?, ?, 10)`,
			fixture.runID, started, started); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`
			INSERT INTO stage_attempts (
				run_id, stage, traversal, attempt, status, started_at, finished_at, duration_ms
			) VALUES (?, 'implement', 1, 1, 'success', ?, ?, 10)`,
			fixture.runID, started, started); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`
			INSERT INTO agent_invocations (
				run_id, span_id, kind, stage, traversal, attempt, model, harness_version
			) VALUES (?, ?, 'task', 'implement', 1, 1, ?, ?)`,
			fixture.runID, fmt.Sprintf("span-%d", i), fixture.model, fixture.version); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, stderr := runArgs(
		t, "telemetry", "stats", "--json",
		"--model=gpt-5.6-sol", "--harness-version=copilot version 1.2.3",
		"--group-by=model", "--group-by=harness-version", root,
	)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var result readservice.TelemetryStatsResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Model != "gpt-5.6-sol" ||
		result.Runs[0].HarnessVersion != "copilot version 1.2.3" ||
		len(result.Stages) != 1 || result.Stages[0].Model != "gpt-5.6-sol" {
		t.Fatalf("provenance stats = %#v", result)
	}
}

func TestTelemetryErrorsJSON(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "errors", "--json", "--rebuild", root)
	if code != 0 {
		t.Fatalf("telemetry errors --json: code = %d, stderr = %q", code, stderr)
	}
	var got []rollup.ErrorEvent
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("telemetry errors --json produced invalid JSON: %v\n%s", err, stdout)
	}
	if len(got) != 1 || got[0].RunID != "fixture-run-1" ||
		got[0].Workflow != "default-implement" || got[0].Stage != "implement" ||
		got[0].Attempt != 1 || got[0].Code != "fixture_error" ||
		got[0].Message != "fixture-injected failure" || got[0].OccurredAt.IsZero() {
		t.Fatalf("errors = %#v", got)
	}
	var documents []json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &documents); err != nil {
		t.Fatal(err)
	}
	assertJSONObjectKeys(t, documents[0],
		"runId", "workflow", "stage", "attempt", "code", "errorClass", "message", "occurredAt",
	)
}

func TestTelemetryJSONEmptyInstance(t *testing.T) {
	root := initDemo(t)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "stats", args: []string{"telemetry", "stats", "--json", root}, want: `{"gaggles":[],"runs":[],"stages":[],"usage":[],"models":[],"curation":{"runs":0,"reportedRuns":0,"ready":0,"needsHuman":0,"closed":0,"deduped":0,"split":0,"stale":0,"reconciled":0,"milestoned":0,"bounced":0},"readyPool":{"claimAgeSamples":0,"forwardCurationThroughput":0,"implementationDemand":0}}` + "\n"},
		{name: "errors", args: []string{"telemetry", "errors", "--json", root}, want: "[]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, test.args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if stdout != test.want {
				t.Fatalf("stdout = %q, want %q", stdout, test.want)
			}
		})
	}
}

type telemetryParityReader struct {
	*readservice.Telemetry
}

func (r *telemetryParityReader) Health(context.Context) (readservice.Health, error) {
	return readservice.Health{Ready: true}, nil
}

func (r *telemetryParityReader) PortalConfig(context.Context) (readservice.PortalConfig, error) {
	return readservice.PortalConfig{}, nil
}

func (r *telemetryParityReader) ListRuns(context.Context, readservice.RunListOptions) (readservice.RunList, error) {
	return readservice.RunList{}, readservice.ErrNotFound
}

func (r *telemetryParityReader) GetRun(context.Context, string) (readservice.RunDetail, error) {
	return readservice.RunDetail{}, readservice.ErrNotFound
}

func (r *telemetryParityReader) RunEvents(context.Context, string) (readservice.EventList, error) {
	return readservice.EventList{}, readservice.ErrNotFound
}

func (r *telemetryParityReader) StageAttempts(context.Context, string, string) (readservice.AttemptList, error) {
	return readservice.AttemptList{}, readservice.ErrNotFound
}

func (r *telemetryParityReader) Artifact(context.Context, string, string) (readservice.ArtifactContent, error) {
	return readservice.ArtifactContent{}, readservice.ErrNotFound
}

func (r *telemetryParityReader) Instance(context.Context) (readservice.Instance, error) {
	return readservice.Instance{}, nil
}

func (r *telemetryParityReader) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	return readservice.GagglePage{}, nil
}

func (r *telemetryParityReader) Goobers(context.Context, string, readservice.PageRequest) (readservice.GooberPage, error) {
	return readservice.GooberPage{}, nil
}

func (r *telemetryParityReader) Workflows(context.Context, string, readservice.PageRequest) (readservice.WorkflowPage, error) {
	return readservice.WorkflowPage{}, nil
}

func (r *telemetryParityReader) Workflow(context.Context, string, string) (readservice.WorkflowDetail, error) {
	return readservice.WorkflowDetail{}, nil
}

func TestTelemetryHTTPAndCLIProjectionParity(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	statsCode, statsJSON, statsStderr := runArgs(
		t,
		"telemetry", "stats", "--json", "--workflow=default-implement", "--gaggle=example", "--rebuild", root,
	)
	if statsCode != 0 {
		t.Fatalf("stats code = %d, stderr = %q", statsCode, statsStderr)
	}
	errorsCode, errorsJSON, errorsStderr := runArgs(
		t,
		"telemetry", "errors", "--json", "--workflow=default-implement", "--gaggle=example", "--limit=1", root,
	)
	if errorsCode != 0 {
		t.Fatalf("errors code = %d, stderr = %q", errorsCode, errorsStderr)
	}

	db, err := rollup.Open(instance.NewLayout(root).TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	telemetry, err := readservice.NewTelemetry(db)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(
		&telemetryParityReader{Telemetry: telemetry},
		httpapi.AllowAll,
		log.New(io.Discard, "", 0),
	)
	if err != nil {
		t.Fatal(err)
	}

	statsResponse := httptest.NewRecorder()
	handler.ServeHTTP(statsResponse, httptest.NewRequest(
		http.MethodGet,
		httpapi.TelemetryStatsPath+"?workflow=default-implement&gaggle=example",
		nil,
	))
	if statsResponse.Code != http.StatusOK {
		t.Fatalf("stats HTTP status = %d, body = %s", statsResponse.Code, statsResponse.Body)
	}
	if statsResponse.Body.String() != statsJSON {
		t.Fatalf("stats HTTP = %s, CLI = %s", statsResponse.Body.String(), statsJSON)
	}

	errorsResponse := httptest.NewRecorder()
	handler.ServeHTTP(errorsResponse, httptest.NewRequest(
		http.MethodGet,
		httpapi.TelemetryErrorsPath+"?workflow=default-implement&gaggle=example&limit=1",
		nil,
	))
	if errorsResponse.Code != http.StatusOK {
		t.Fatalf("errors HTTP status = %d, body = %s", errorsResponse.Code, errorsResponse.Body)
	}
	var page readservice.TelemetryErrorsPage
	if err := json.NewDecoder(errorsResponse.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	var cliErrors []readservice.TelemetryError
	if err := json.Unmarshal([]byte(errorsJSON), &cliErrors); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Items, cliErrors) {
		t.Fatalf("HTTP errors = %+v, CLI errors = %+v", page.Items, cliErrors)
	}
}

func TestTelemetryStatsKeepsMissingMetricsUnknown(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	run, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           "1111111111111111aaaaaaaaaaaaaaaa",
		Workflow:        "active-workflow",
		WorkflowVersion: 1,
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "active", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "telemetry", "stats", "--json", "--rebuild", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var document struct {
		Runs   []map[string]json.RawMessage `json:"runs"`
		Stages []map[string]json.RawMessage `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatal(err)
	}
	for _, item := range append(document.Runs, document.Stages...) {
		for _, metric := range []string{"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs"} {
			if _, ok := item[metric]; ok {
				t.Fatalf("unknown metric %q was serialized: %s", metric, stdout)
			}
		}
	}

	code, stdout, stderr = runArgs(t, "telemetry", "stats", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Fatalf("stdout = %q, want unknown metric presentation", stdout)
	}
}

func TestTelemetryRejectsInvalidTimeWindow(t *testing.T) {
	code, _, stderr := runArgs(
		t,
		"telemetry", "stats",
		"--since=2026-07-02T00:00:00Z",
		"--until=2026-07-01T00:00:00Z",
	)
	if code != 2 || !strings.Contains(stderr, "--since must not be after --until") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func TestTelemetryExportRequiresSince(t *testing.T) {
	code, stdout, stderr := runArgs(t, "telemetry", "export")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--since is required") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestTelemetryExportRejectsMissingInstanceRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")

	code, stdout, stderr := runArgs(t, "telemetry", "export", "--since=2026-07-21T00:00:00Z", root)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestTelemetryExportClassifiesStagingWriteFailureAsOutputError(t *testing.T) {
	root := initDemo(t)
	writeErr := errors.New("fixture staging write failure")
	export := func([]string, time.Time, time.Time, io.Writer) error {
		return &telemetry.ExportOutputError{Err: writeErr}
	}

	var stdout, stderr strings.Builder
	code := runTelemetryExportWithExporter(
		[]string{"--since=2026-07-21T00:00:00Z", root},
		&stdout,
		&stderr,
		export,
	)
	if code != 2 || stdout.String() != "" || !strings.Contains(stderr.String(), writeErr.Error()) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestTelemetryExportEmitsWindowAndDoesNotEmitPartialOutputOnCorruptJournal(t *testing.T) {
	root := initDemo(t)
	runsDir := instance.NewLayout(root).RunsDir()
	validPath := filepath.Join(runsDir, "a-valid", "spans", "otlp.jsonl")
	corruptPath := filepath.Join(runsDir, "b-corrupt", "spans", "otlp.jsonl")
	if err := os.MkdirAll(filepath.Dir(validPath), 0o755); err != nil {
		t.Fatal(err)
	}
	valid := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"11111111111111111111111111111111","spanId":"2222222222222222","name":"valid","startTimeUnixNano":"1784656800000000000","endTimeUnixNano":"1784656801000000000"}]}]}]}` + "\n"
	if err := os.WriteFile(validPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "telemetry", "export", "--since=2026-07-21T00:00:00Z", root)
	if code != 0 || stdout != valid || stderr != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = runArgs(t, "telemetry", "export", "--since=2026-07-21T00:00:00Z", root)
	if code != 1 || stdout != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `run "b-corrupt"`) || !strings.Contains(stderr, "corrupt OTLP journal record 1") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestTelemetryStatsWithoutRebuildMissesOutOfBandRun is issue #127's core
// contract change: a query no longer force-rebuilds (os.Remove + full
// rescan) on every call — that was the "two concurrent CLI queries unlink
// each other mid-ingest" defect. A run journaled out-of-band (no incremental
// ingest hook ever ran for it) is invisible to a plain query; --rebuild is
// required to discover it. This is the negative-space proof that
// TestTelemetryStatsAfterRun's --rebuild flag is load-bearing, not
// decorative.
func TestTelemetryStatsWithoutRebuildMissesOutOfBandRun(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "stats", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no runs found") {
		t.Fatalf("stdout = %q, want the out-of-band run to be invisible without --rebuild", stdout)
	}
}

func TestTelemetryStatsEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "telemetry", "stats", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no runs found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTelemetryErrorsEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "telemetry", "errors", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no errors found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTelemetryNoSubcommand(t *testing.T) {
	code, _, stderr := runArgs(t, "telemetry")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestTelemetryUnknownSubcommand(t *testing.T) {
	code, _, stderr := runArgs(t, "telemetry", "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func assertJSONObjectKeys(t *testing.T, data []byte, expected ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	if len(object) != len(expected) {
		t.Fatalf("keys = %v, want %v", object, expected)
	}
	for _, key := range expected {
		if _, ok := object[key]; !ok {
			t.Fatalf("JSON object missing key %q: %s", key, data)
		}
	}
}
