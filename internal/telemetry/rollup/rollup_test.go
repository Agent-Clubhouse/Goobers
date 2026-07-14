package rollup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/telemetry"
)

var fixtureStart = time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)

func openTestDB(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(filepath.Join(dir, "telemetry.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestIngestRunMatchesJournalEvents is #22's headline acceptance criterion:
// after a fixture run, rollup rows match the journal events exactly.
func TestIngestRunMatchesJournalEvents(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runDir := writeFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	db := openTestDB(t, tmp)

	if err := db.IngestRun(runDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	runs, err := db.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.RunID != fixtureRunID || r.Workflow != "implement" || r.WorkflowVersion != 3 ||
		r.WorkflowDigest != "sha256:deadbeefcafef00d" || r.Gaggle != "web" ||
		r.TriggerKind != "item" || r.TriggerRef != "issue-42" || r.Status != "failed" {
		t.Fatalf("unexpected run row: %#v", r)
	}
	if !r.StartedAt.Equal(fixtureStart) {
		t.Fatalf("StartedAt = %v, want %v", r.StartedAt, fixtureStart)
	}
	wantFinished := fixtureStart.Add(9 * time.Second)
	if !r.FinishedAt.Equal(wantFinished) {
		t.Fatalf("FinishedAt = %v, want %v", r.FinishedAt, wantFinished)
	}
	if r.DurationMs != 9000 {
		t.Fatalf("DurationMs = %d, want 9000", r.DurationMs)
	}

	stages, err := db.StageAttempts(fixtureRunID)
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2 (build, deploy)", len(stages))
	}
	build, deploy := stages[0], stages[1]
	if build.Stage != "build" || build.Status != "succeeded" || build.AttemptClass != "policy" {
		t.Fatalf("unexpected build stage row: %#v", build)
	}
	if build.DurationMs != 2000 { // finished at +3s, started at +1s
		t.Fatalf("build DurationMs = %d, want 2000", build.DurationMs)
	}
	if deploy.Stage != "deploy" || deploy.Status != "failed" {
		t.Fatalf("unexpected deploy stage row: %#v", deploy)
	}
	if deploy.ErrorCode != "provider.rate_limit" {
		t.Fatalf("deploy ErrorCode = %q, want provider.rate_limit", deploy.ErrorCode)
	}
	if deploy.ErrorClass != string(telemetry.ErrorClassProviderRateLimit) {
		t.Fatalf("deploy ErrorClass = %q, want %q", deploy.ErrorClass, telemetry.ErrorClassProviderRateLimit)
	}

	gates, err := db.GateVerdicts(fixtureRunID)
	if err != nil {
		t.Fatalf("GateVerdicts: %v", err)
	}
	if len(gates) != 1 || gates[0].Gate != "review" || gates[0].Verdict != "approve" || gates[0].Target != "deploy" {
		t.Fatalf("unexpected gate verdicts: %#v", gates)
	}

	muts, err := db.ProviderMutations(fixtureRunID)
	if err != nil {
		t.Fatalf("ProviderMutations: %v", err)
	}
	if len(muts) != 1 {
		t.Fatalf("len(muts) = %d, want 1", len(muts))
	}
	m := muts[0]
	if m.Provider != "github" || m.Kind != "issue" || m.ExternalID != "42" ||
		m.URL != "https://github.com/acme/app/issues/42" || m.Operation != "claim" {
		t.Fatalf("unexpected provider mutation: %#v", m)
	}

	errs, err := db.RunErrors(fixtureRunID)
	if err != nil {
		t.Fatalf("RunErrors: %v", err)
	}
	if len(errs) != 1 || errs[0].Code != "provider.rate_limit" || errs[0].Stage != "deploy" || errs[0].Attempt != 1 {
		t.Fatalf("unexpected run errors: %#v", errs)
	}
	if errs[0].ErrorClass != string(telemetry.ErrorClassProviderRateLimit) {
		t.Fatalf("run_error ErrorClass = %q, want provider-rate-limit", errs[0].ErrorClass)
	}
}

// TestSeededFailingRunYieldsRightErrorClass covers the acceptance criterion
// with additional error codes beyond the main fixture, including the
// heuristic-fallback and unknown paths.
func TestSeededFailingRunYieldsRightErrorClass(t *testing.T) {
	cases := []struct {
		code string
		want telemetry.ErrorClass
	}{
		{"provider.rate_limit", telemetry.ErrorClassProviderRateLimit},
		{"harness.crash", telemetry.ErrorClassHarnessFailure},
		{"timeout", telemetry.ErrorClassTimeout},
		{"validation.failed", telemetry.ErrorClassValidation},
		{"something-nobody-registered", telemetry.ErrorClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			tmp := t.TempDir()
			runsDir := filepath.Join(tmp, "runs")
			runID := fixtureRunID
			dir := filepath.Join(runsDir, runID)
			mustMkdirAll(t, dir)
			mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(runID, fixtureStart))
			events := strings.Join([]string{
				eventLine(1, fixtureStart, `"type":"run.started"`),
				eventLine(2, fixtureStart.Add(time.Second), `"type":"stage.started","stage":"s","attempt":1`),
				eventLine(3, fixtureStart.Add(2*time.Second), `"type":"error","stage":"s","attempt":1,"error":{"code":"`+tc.code+`"}`),
				eventLine(4, fixtureStart.Add(3*time.Second), `"type":"stage.finished","stage":"s","attempt":1,"status":"failed"`),
				eventLine(5, fixtureStart.Add(4*time.Second), `"type":"run.finished","status":"failed"`),
			}, "\n") + "\n"
			mustWriteFile(t, filepath.Join(dir, fileEvents), events)

			db := openTestDB(t, tmp)
			if err := db.IngestRun(dir); err != nil {
				t.Fatalf("IngestRun: %v", err)
			}
			stages, err := db.StageAttempts(runID)
			if err != nil || len(stages) != 1 {
				t.Fatalf("StageAttempts: %v, %#v", err, stages)
			}
			if got := stages[0].ErrorClass; got != string(tc.want) {
				t.Fatalf("ErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRebuildIsReproducible: delete telemetry.db, rebuild from journals,
// query results must be identical to incremental per-run ingestion.
func TestRebuildIsReproducible(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir1 := writeFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	dir2 := writeMinimalFixtureRun(t, runsDir, fixtureRunID2, fixtureStart.Add(time.Hour))

	incrementalPath := filepath.Join(tmp, "incremental.db")
	incDB, err := Open(incrementalPath)
	if err != nil {
		t.Fatalf("Open incremental: %v", err)
	}
	if err := incDB.IngestRun(dir1); err != nil {
		t.Fatalf("IngestRun dir1: %v", err)
	}
	if err := incDB.IngestRun(dir2); err != nil {
		t.Fatalf("IngestRun dir2: %v", err)
	}
	incResult := snapshotDB(t, incDB)
	if err := incDB.Close(); err != nil {
		t.Fatalf("close incremental: %v", err)
	}

	rebuildPath := filepath.Join(tmp, "rebuilt.db")
	if err := Rebuild(rebuildPath, runsDir, filepath.Join(tmp, "scheduler")); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	rebuiltDB, err := Open(rebuildPath)
	if err != nil {
		t.Fatalf("Open rebuilt: %v", err)
	}
	rebuiltResult := snapshotDB(t, rebuiltDB)
	_ = rebuiltDB.Close()

	if incResult != rebuiltResult {
		t.Fatalf("rebuild result differs from incremental ingest:\nincremental=%s\nrebuilt=%s", incResult, rebuiltResult)
	}

	// Rebuilding again (delete + rebuild) over the same journals must be
	// byte-identical to the first rebuild.
	if err := Rebuild(rebuildPath, runsDir, filepath.Join(tmp, "scheduler")); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	rebuiltDB2, err := Open(rebuildPath)
	if err != nil {
		t.Fatalf("Open rebuilt (2nd): %v", err)
	}
	secondResult := snapshotDB(t, rebuiltDB2)
	_ = rebuiltDB2.Close()
	if rebuiltResult != secondResult {
		t.Fatalf("rebuild is not idempotent:\nfirst=%s\nsecond=%s", rebuiltResult, secondResult)
	}
}

// snapshotDB renders every table's query results (across both fixture runs)
// as deterministic, ordered JSON — the basis for the byte-identical
// comparison. Queries are already ordered by stable keys (see query.go).
func snapshotDB(t *testing.T, db *DB) string {
	t.Helper()
	runs, err := db.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	snap := map[string]any{"runs": runs}
	for _, r := range runs {
		stages, err := db.StageAttempts(r.RunID)
		if err != nil {
			t.Fatalf("StageAttempts(%s): %v", r.RunID, err)
		}
		gates, err := db.GateVerdicts(r.RunID)
		if err != nil {
			t.Fatalf("GateVerdicts(%s): %v", r.RunID, err)
		}
		muts, err := db.ProviderMutations(r.RunID)
		if err != nil {
			t.Fatalf("ProviderMutations(%s): %v", r.RunID, err)
		}
		errs, err := db.RunErrors(r.RunID)
		if err != nil {
			t.Fatalf("RunErrors(%s): %v", r.RunID, err)
		}
		snap["stages/"+r.RunID] = stages
		snap["gates/"+r.RunID] = gates
		snap["muts/"+r.RunID] = muts
		snap["errs/"+r.RunID] = errs
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(b)
}

// TestCanarySecretRedactedInRollup verifies redaction at this layer too
// (defense in depth with #8/#14): a canary secret embedded in an error
// message and in a runner.* annotation must never land in the rollup.
func TestCanarySecretRedactedInRollup(t *testing.T) {
	const canary = "ghp_0123456789abcdefghijklmnopqrstuvwx"
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runID := fixtureRunID
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(runID, fixtureStart))
	events := strings.Join([]string{
		eventLine(1, fixtureStart, `"type":"run.started"`),
		eventLine(2, fixtureStart.Add(time.Second), `"type":"stage.started","stage":"s","attempt":1`),
		eventLine(3, fixtureStart.Add(2*time.Second), `"type":"error","stage":"s","attempt":1,"error":{"code":"harness.failure","message":"leaked token `+canary+`"}`),
		eventLine(4, fixtureStart.Add(3*time.Second), `"type":"ref.touched","externalRef":{"provider":"github","kind":"issue","id":"1"},"runner":{"operation":"update","authHeader":"Bearer `+canary+`"}`),
		eventLine(5, fixtureStart.Add(4*time.Second), `"type":"stage.finished","stage":"s","attempt":1,"status":"failed"`),
		eventLine(6, fixtureStart.Add(5*time.Second), `"type":"run.finished","status":"failed"`),
	}, "\n") + "\n"
	mustWriteFile(t, filepath.Join(dir, fileEvents), events)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	errs, err := db.RunErrors(runID)
	if err != nil || len(errs) != 1 {
		t.Fatalf("RunErrors: %v, %#v", err, errs)
	}
	if strings.Contains(errs[0].Message, canary) {
		t.Fatalf("canary leaked into run_errors.message: %q", errs[0].Message)
	}
	stages, err := db.StageAttempts(runID)
	if err != nil || len(stages) != 1 {
		t.Fatalf("StageAttempts: %v, %#v", err, stages)
	}
	if strings.Contains(stages[0].ErrorCode, canary) {
		t.Fatalf("canary leaked into stage_attempts.error_code: %q", stages[0].ErrorCode)
	}

	muts, err := db.ProviderMutations(runID)
	if err != nil || len(muts) != 1 {
		t.Fatalf("ProviderMutations: %v, %#v", err, muts)
	}
	// runner_json isn't exposed via the query API (it's a passthrough debug
	// column), so inspect it directly.
	var runnerJSONCol string
	if err := db.sql.QueryRow(`SELECT runner_json FROM provider_mutations WHERE run_id = ?`, runID).Scan(&runnerJSONCol); err != nil {
		t.Fatalf("query runner_json: %v", err)
	}
	if strings.Contains(runnerJSONCol, canary) {
		t.Fatalf("canary leaked into provider_mutations.runner_json: %q", runnerJSONCol)
	}
}

// TestWithinStageSpanEventsSurviveRollup: a fixture agentic run's harness
// events (span events) appear attached to the stage span under spans/ and
// survive rollup — queries return both stage-level (spans) and within-stage
// (span_events) rows.
func TestWithinStageSpanEventsSurviveRollup(t *testing.T) {
	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runDir := writeMinimalFixtureRun(t, runsDir, runID, fixtureStart)

	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "goobers-test",
		SpanExporter: telemetry.NewJournalSpanExporter(runsDir),
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	ctx, task, err := client.StartTask(context.Background(), telemetry.TaskAttributes{
		Gaggle: "web", WorkflowID: "nominate", WorkflowVersion: "1", RunID: runID, TaskID: "scan", TaskType: "agentic",
	})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	_ = ctx
	task.Event("harness.tool_call", attribute.String("tool", "grep"))
	task.Event("harness.model_response", attribute.String("tokens", "128"))
	task.Succeed("scan complete")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(runDir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	spans, err := db.Spans(runID)
	if err != nil {
		t.Fatalf("Spans: %v", err)
	}
	if len(spans) != 1 || spans[0].Kind != telemetry.SpanKindTask || spans[0].Status != "ok" {
		t.Fatalf("unexpected spans: %#v", spans)
	}

	events, err := db.SpanEvents(runID, spans[0].SpanID)
	if err != nil {
		t.Fatalf("SpanEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(span events) = %d, want 2", len(events))
	}
	if events[0].Name != "harness.tool_call" || events[0].Attributes["tool"] != "grep" {
		t.Fatalf("unexpected span event 0: %#v", events[0])
	}
	if events[1].Name != "harness.model_response" || events[1].Attributes["tokens"] != "128" {
		t.Fatalf("unexpected span event 1: %#v", events[1])
	}
}

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "telemetry.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent migrate): %v", err)
	}
	defer func() { _ = db2.Close() }()
	var version int
	if err := db2.sql.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatalf("read schema_meta: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema_meta.version = %d, want %d", version, len(migrations))
	}
}

func minimalRunYAML(runID string, startedAt time.Time) string {
	return "schema: goobers.dev/journal/run/v1\nrunId: " + runID +
		"\nworkflow: wf\nworkflowVersion: 1\ngaggle: web\ntrigger:\n  kind: manual\nstartedAt: " +
		startedAt.UTC().Format(time.RFC3339) + "\n"
}

func eventLine(seq int, ts time.Time, rest string) string {
	return `{"schema":"goobers.dev/journal/event/v1","seq":` + strconv.Itoa(seq) + `,"branch":0,"time":"` + ts.UTC().Format(time.RFC3339Nano) + `",` + rest + "}"
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
