package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// TestTelemetryQueryEmitsSignals seeds a rollup with one failed run and asserts
// telemetry-query emits parseable signals JSON covering per-workflow stats and
// the run's error signature over the window (#232).
func TestTelemetryQueryEmitsSignals(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root) // run "fixture-run-1": stage "implement" fails, error code "fixture_error"

	l := instance.NewLayout(root)
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	code, stdout, stderr := runArgs(t, "telemetry-query", "--window", "168h", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}

	var got telemetryQueryResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.Schema != telemetryQuerySchema {
		t.Fatalf("schema = %q, want %q", got.Schema, telemetryQuerySchema)
	}
	if len(got.Workflows) == 0 {
		t.Fatalf("expected at least one per-workflow signal: %+v", got)
	}
	var sawSig bool
	for _, s := range got.ErrorSignatures {
		if s.Code == "fixture_error" {
			sawSig = true
		}
	}
	if !sawSig {
		t.Fatalf("expected the fixture run's error signature in signals: %+v", got.ErrorSignatures)
	}
	if got.NoWork || got.Note != "" {
		t.Fatalf("populated signals unexpectedly reported no-work: %+v", got)
	}
}

func TestTelemetryQueryMissingRollupIsNoWork(t *testing.T) {
	root := initDemo(t)
	if err := os.Remove(instance.NewLayout(root).TelemetryDB()); err != nil {
		t.Fatal(err)
	}
	assertTelemetryQueryNoWork(t, root, "24h", telemetryQueryNoRollupNote)
}

func TestTelemetryQueryFreshRollupIsNoWork(t *testing.T) {
	assertTelemetryQueryNoWork(t, initDemo(t), "24h", telemetryQueryEmptyWindowNote)
}

func TestTelemetryQueryEmptyWindowIsNoWork(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	l := instance.NewLayout(root)
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	assertTelemetryQueryNoWork(t, root, "1ns", telemetryQueryEmptyWindowNote)
}

func TestTelemetryQueryCorruptRollupIsError(t *testing.T) {
	root := initDemo(t)
	if err := os.WriteFile(instance.NewLayout(root).TelemetryDB(), []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "telemetry-query", "--window", "24h", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "open telemetry rollup") {
		t.Fatalf("stderr = %q, want corrupt rollup error", stderr)
	}
}

func assertTelemetryQueryNoWork(t *testing.T, root, window, wantNote string) {
	t.Helper()
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), "telemetry-signals.json")

	code, stdout, stderr := runArgs(t, "telemetry-query", "--window", window, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want result written only to declared resultFile", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "telemetry-signals.json"))
	if err != nil {
		t.Fatalf("read telemetry-signals.json: %v", err)
	}
	var got telemetryQueryResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal telemetry-signals.json: %v", err)
	}
	if !got.NoWork {
		t.Fatalf("noWork = false, want true: %s", data)
	}
	if got.Note != wantNote {
		t.Fatalf("note = %q, want %q", got.Note, wantNote)
	}
}

func TestTelemetryQueryUnknownFlagIsUsageError(t *testing.T) {
	code, _, _ := runArgs(t, "telemetry-query", "--bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage/IO error)", code)
	}
}
