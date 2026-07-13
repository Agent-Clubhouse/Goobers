package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func initDemo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}
	return root
}

func TestRunTriggersManualRunEscalated(t *testing.T) {
	root := initDemo(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "created run ") {
		t.Fatalf("run stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Fatalf("expected an honest escalation note (no runner yet), stdout = %q", stdout)
	}

	// status lists the run, escalated.
	code, stdout, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "default-implement") || !strings.Contains(stdout, "escalated") {
		t.Fatalf("status stdout = %q", stdout)
	}

	// trace shows the run.started / error / run.finished sequence.
	runID := strings.Fields(strings.Split(stdout, "\n")[1])[0]
	code, stdout, stderr = runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"run.started", "runner_unavailable", "run.finished status=escalated"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace stdout missing %q: %q", want, stdout)
		}
	}
}

func TestRunUnknownWorkflow(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "run", "no-such-workflow", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `no workflow named "no-such-workflow"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunMissingInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "run", "anything", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestStatusEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no runs found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTraceUnknownRun(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "trace", "bogus-run-id", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stderr == "" {
		t.Fatalf("expected an error message on stderr")
	}
}

func TestTraceTooFewArgs(t *testing.T) {
	code, _, _ := runArgs(t, "trace")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestUpValidInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, _ := runArgs(t, "up", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (daemon not yet wired)", code)
	}
	if !strings.Contains(stdout, "not yet wired") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUpMissingInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "up", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}
