package main

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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

func TestTelemetryStatsAfterRun(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "stats", root)
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

	code, stdout, stderr := runArgs(t, "telemetry", "errors", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "fixture_error") {
		t.Fatalf("stdout = %q", stdout)
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
