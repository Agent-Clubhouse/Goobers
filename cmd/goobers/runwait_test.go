package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// TestWaitForRunTerminalFailsFastOnDeadline is the #827 recurrence guard for the
// bounded wait: when runTerminalWaitTimeout is set (as the suite does in
// TestMain), a run that never reaches a terminal phase must make
// waitForRunTerminal return an ERROR promptly, not poll forever. Pre-guard a
// wedged run hung the whole 10-minute local-ci stage with no signal; this keeps
// that a fast, diagnosable failure instead.
func TestWaitForRunTerminalFailsFastOnDeadline(t *testing.T) {
	prev := runTerminalWaitTimeout
	runTerminalWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { runTerminalWaitTimeout = prev })

	root := t.TempDir()
	const runID = "stuck-nonterminal"
	// A run left in the (non-terminal) running phase — the shape a wedged
	// dispatch goroutine leaves behind.
	writeStatusRunWithPhase(t, root, runID, "default-implement", "example", time.Now(), journal.PhaseRunning)

	done := make(chan struct{})
	var phase journal.RunPhase
	var err error
	go func() {
		phase, err = waitForRunTerminal(context.Background(), instance.NewLayout(root).RunsDir(), runID)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("waitForRunTerminal did not return after its deadline — the bounded-wait guard is not in effect")
	}
	if err == nil {
		t.Fatalf("waitForRunTerminal returned nil error for a run that never reached terminal (phase=%s); want a deadline error", phase)
	}
	if !strings.Contains(err.Error(), "terminal phase") {
		t.Errorf("error = %q, want it to explain the missed terminal phase", err)
	}
}

// TestWaitForRunTerminalUnboundedByDefault pins that production behavior is
// unchanged: with runTerminalWaitTimeout at its zero default, a still-running run
// is polled (not failed) — the guard is strictly opt-in, so a human's
// `goobers run` still waits indefinitely until the run finishes or they Ctrl-C.
func TestWaitForRunTerminalUnboundedByDefault(t *testing.T) {
	prev := runTerminalWaitTimeout
	runTerminalWaitTimeout = 0
	t.Cleanup(func() { runTerminalWaitTimeout = prev })

	root := t.TempDir()
	const runID = "still-running"
	writeStatusRunWithPhase(t, root, runID, "default-implement", "example", time.Now(), journal.PhaseRunning)

	// With no deadline, the only way out is ctx cancellation — confirm it polls
	// until then rather than returning an error on its own.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var err error
	go func() {
		_, err = waitForRunTerminal(ctx, instance.NewLayout(root).RunsDir(), runID)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("waitForRunTerminal returned before cancellation with no deadline set — it must wait indefinitely in production")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	<-done
	// A signal-style cancel (no deadline) reports the current phase with no error.
	if err != nil {
		t.Errorf("after cancel with no deadline, err = %v, want nil (report current phase like a Ctrl-C)", err)
	}
}
