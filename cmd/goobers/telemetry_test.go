package main

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           "fixture-run-1",
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          "example",
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
	if !strings.Contains(stdout, "WORKFLOW STATS") || !strings.Contains(stdout, "default-implement") {
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
	if len(got.Stages) != 1 || got.Stages[0].Stage != "implement" ||
		got.Stages[0].TotalAttempts != 1 || got.Stages[0].FailedAttempts != 1 {
		t.Fatalf("stage stats = %#v", got.Stages)
	}

	var document struct {
		Runs   []json.RawMessage `json:"runs"`
		Stages []json.RawMessage `json:"stages"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatal(err)
	}
	assertJSONObjectKeys(t, document.Runs[0],
		"workflow", "totalRuns", "completedRuns", "failedRuns", "otherRuns",
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
	)
	assertJSONObjectKeys(t, document.Stages[0],
		"stage", "totalAttempts", "succeededAttempts", "failedAttempts",
		"successRate", "avgDurationMs", "minDurationMs", "maxDurationMs",
	)
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
		{name: "stats", args: []string{"telemetry", "stats", "--json", root}, want: `{"runs":[],"stages":[]}` + "\n"},
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
