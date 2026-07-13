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

// TestRunCompletesDeterministicWorkflow exercises `goobers run` end to end
// against the real runner (issue #23's daemon-loop follow-up rewired both
// `run` and `up` off the old escalation stub) — a deterministic-only fixture
// workflow so it needs neither a real Copilot CLI nor network access; see
// initDeterministicDemo in daemon_test.go.
func TestRunCompletesDeterministicWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "created run ") {
		t.Fatalf("run stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("expected the real runner to complete the deterministic task, stdout = %q", stdout)
	}

	// status lists the run, completed.
	code, stdout, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "default-implement") || !strings.Contains(stdout, "completed") {
		t.Fatalf("status stdout = %q", stdout)
	}

	// trace shows the real stage dispatch sequence.
	runID := strings.Fields(strings.Split(stdout, "\n")[1])[0]
	code, stdout, stderr = runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"run.started", "stage.started", "local-ci", "stage.finished", "run.finished status=completed"} {
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

// TestUpValidInstance-equivalent coverage (a valid instance's daemon starts,
// idles, and drains cleanly) now lives in daemon_test.go's
// TestUpIdlesThenDrainsOnCancel — `up` blocks on the real daemon loop, so it
// needs a cancellable context (runUpContext) rather than runArgs' synchronous
// signal-only runUp.

func TestUpMissingInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "up", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}
