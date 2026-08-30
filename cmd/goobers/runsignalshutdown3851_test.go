package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// #3851: `run` and `signal` used to print a shutdown failure (lost telemetry
// flush or store/log close) but still return the command's own success exit
// code, contrary to the issue's requirement not to report clean completion
// after losing final persisted state. These tests drive the full command —
// real instance, real scheduler setup, real Shutdown() — through
// testInjectedShutdownStepErr (daemon.go) so the propagation is exercised at
// the command's own result, not just at the lower-level Shutdown() unit
// tested in daemonshutdown3651_test.go.

func withInjectedShutdownFailure(t *testing.T) {
	t.Helper()
	restore := testInjectedShutdownStepErr
	testInjectedShutdownStepErr = errors.New("injected shutdown failure")
	t.Cleanup(func() { testInjectedShutdownStepErr = restore })
}

func TestRunFailsCommandResultWhenShutdownFails(t *testing.T) {
	root := initTerminalPhaseDemo(t, journal.PhaseCompleted, false)
	withInjectedShutdownFailure(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (a failed shutdown must not report success); stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "phase="+string(journal.PhaseCompleted)) {
		t.Fatalf("stdout = %q, want the run's own completed phase still reported", stdout)
	}
	if !strings.Contains(stderr, "shut down scheduler services") {
		t.Fatalf("stderr = %q, want the shutdown failure diagnostic", stderr)
	}
}

func TestRunSucceedsWhenShutdownSucceeds(t *testing.T) {
	root := initTerminalPhaseDemo(t, journal.PhaseCompleted, false)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

// A shutdown failure never masks a run outcome that already failed — it must
// not downgrade exit code 3 (escalated) or 1 (failed/aborted) to something
// else, and the shutdown diagnostic still prints alongside it.
func TestRunShutdownFailureDoesNotMaskExistingFailureExitCode(t *testing.T) {
	root := initTerminalPhaseDemo(t, journal.PhaseEscalated, false)
	withInjectedShutdownFailure(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 3 {
		t.Fatalf("code = %d, want 3 (escalation must take precedence over a shutdown failure); stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "shut down scheduler services") {
		t.Fatalf("stderr = %q, want the shutdown failure diagnostic even when the run's own result was already non-zero", stderr)
	}
}

func TestSignalFailsCommandResultWhenShutdownFails(t *testing.T) {
	root := initTerminalPhaseDemo(t, journal.PhaseCompleted, true)
	withInjectedShutdownFailure(t)

	code, stdout, stderr := runArgs(t, "signal", "deploy", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (a failed shutdown must not report success); stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "phase="+string(journal.PhaseCompleted)) {
		t.Fatalf("stdout = %q, want the run's own completed phase still reported", stdout)
	}
	if !strings.Contains(stderr, "shut down scheduler services") {
		t.Fatalf("stderr = %q, want the shutdown failure diagnostic", stderr)
	}
}

func TestSignalSucceedsWhenShutdownSucceeds(t *testing.T) {
	root := initTerminalPhaseDemo(t, journal.PhaseCompleted, true)

	code, stdout, stderr := runArgs(t, "signal", "deploy", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout = %q, stderr = %q", code, stdout, stderr)
	}
}
