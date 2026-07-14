package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

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
}

// TestTelemetryQueryMissingRollupIsError proves a missing rollup file is a
// business error (exit 1) with an actionable message, not a hollow success.
func TestTelemetryQueryMissingRollupIsError(t *testing.T) {
	root := initDemo(t)
	// Simulate telemetry never set up / DB genuinely absent: init scaffolds a
	// 0-byte telemetry.db placeholder, so remove it to get the missing case.
	if err := os.Remove(instance.NewLayout(root).TelemetryDB()); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "telemetry-query", "--window", "24h", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "no telemetry rollup") {
		t.Fatalf("stderr = %q, want an actionable 'no telemetry rollup' message", stderr)
	}
}

// TestTelemetryQueryEmptyRollupSucceeds proves an empty (but present) rollup is
// a valid empty answer (exit 0) — so gather-signals reaches the next stage even
// on a fresh instance with no prior runs in the window.
func TestTelemetryQueryEmptyRollupSucceeds(t *testing.T) {
	// A fresh instance's 0-byte telemetry.db placeholder opens as an empty
	// rollup — no runs yet, but a valid (empty) answer, not an error.
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "telemetry-query", "--window", "24h", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got telemetryQueryResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("empty-rollup output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.Schema != telemetryQuerySchema {
		t.Fatalf("schema = %q, want %q", got.Schema, telemetryQuerySchema)
	}
}

func TestTelemetryQueryUnknownFlagIsUsageError(t *testing.T) {
	code, _, _ := runArgs(t, "telemetry-query", "--bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage/IO error)", code)
	}
}
