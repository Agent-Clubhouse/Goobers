package main

import (
	"strings"
	"testing"
)

func TestTelemetryStatsAfterRun(t *testing.T) {
	root := initDemo(t)
	if code, _, stderr := runArgs(t, "run", "default-implement", root); code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}

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
	if code, _, stderr := runArgs(t, "run", "default-implement", root); code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := runArgs(t, "telemetry", "errors", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "runner_unavailable") {
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
