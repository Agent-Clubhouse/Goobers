package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestWaitForRunTerminalReportsTransitionsPauseAndHeartbeat(t *testing.T) {
	oldPoll := runPollInterval
	oldHeartbeat := runWaitHeartbeatInterval
	runPollInterval = 5 * time.Millisecond
	runWaitHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		runPollInterval = oldPoll
		runWaitHeartbeatInterval = oldHeartbeat
	})

	root := t.TempDir()
	runsDir := instance.NewLayout(root).RunsDir()
	const runID = "progress-run"
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "fixture", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, StartedAt: now,
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = run.Close() }()

	run.SetMachineState("build")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}); err != nil {
		t.Fatalf("append stage start: %v", err)
	}

	var progress synchronizedBuffer
	done := make(chan struct{})
	var phase journal.RunPhase
	var waitErr error
	go func() {
		phase, waitErr = waitForRunTerminalWithProgress(context.Background(), runsDir, runID, &progress)
		close(done)
	}()

	waitForProgress(t, &progress, "stage build started")
	now = now.Add(2 * time.Second)
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: "success"}); err != nil {
		t.Fatalf("append stage finish: %v", err)
	}
	run.SetMachineState("approval")
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
		t.Fatalf("append gate pause: %v", err)
	}

	waitForProgress(t, &progress, "waiting: run progress-run paused at gate approval")
	waitForProgress(t, &progress, "has no new transition")
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait did not stop after the run became terminal")
	}
	if waitErr != nil || phase != journal.PhaseCompleted {
		t.Fatalf("wait = (%s, %v), want completed", phase, waitErr)
	}

	output := progress.String()
	for _, want := range []string{
		"stage build started (run=progress-run, attempt=1, elapsed=0s)",
		"stage build finished (run=progress-run, attempt=1, status=success, elapsed=2s)",
		"waiting: run progress-run paused at gate approval (elapsed=2s)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("progress missing %q:\n%s", want, output)
		}
	}
	if got := strings.Count(output, "stage build started"); got != 1 {
		t.Errorf("stage start emitted %d times, want once:\n%s", got, output)
	}
	if got := strings.Count(output, "stage build finished"); got != 1 {
		t.Errorf("stage finish emitted %d times, want once:\n%s", got, output)
	}
	if got := strings.Count(output, "waiting: run progress-run paused at gate approval"); got != 1 {
		t.Errorf("gate pause emitted %d times, want once:\n%s", got, output)
	}
	if strings.Contains(output, "healthy") {
		t.Errorf("wall-clock heartbeat must not claim stage health:\n%s", output)
	}

	time.Sleep(3 * runWaitHeartbeatInterval)
	if after := progress.String(); after != output {
		t.Errorf("progress continued after terminal state:\nbefore=%q\nafter=%q", output, after)
	}
}

func TestRunWaitReporterRateLimitsHeartbeats(t *testing.T) {
	oldHeartbeat := runWaitHeartbeatInterval
	runWaitHeartbeatInterval = 30 * time.Second
	t.Cleanup(func() { runWaitHeartbeatInterval = oldHeartbeat })

	var progress synchronizedBuffer
	reporter := newRunWaitReporter("rate-limited", &progress)
	started := reporter.lastHeartbeat
	reporter.heartbeat(started.Add(runWaitHeartbeatInterval))
	reporter.heartbeat(started.Add(runWaitHeartbeatInterval + time.Second))
	if got := strings.Count(progress.String(), "has no new transition"); got != 1 {
		t.Fatalf("heartbeats inside one interval = %d, want 1: %q", got, progress.String())
	}
	reporter.heartbeat(started.Add(2 * runWaitHeartbeatInterval))
	if got := strings.Count(progress.String(), "has no new transition"); got != 2 {
		t.Fatalf("heartbeats across two intervals = %d, want 2: %q", got, progress.String())
	}
}

func TestRunWaitReporterDoesNotHeartbeatAfterTerminalEvent(t *testing.T) {
	oldHeartbeat := runWaitHeartbeatInterval
	runWaitHeartbeatInterval = time.Second
	t.Cleanup(func() { runWaitHeartbeatInterval = oldHeartbeat })

	var progress synchronizedBuffer
	reporter := newRunWaitReporter("finished", &progress)
	reporter.observe([]journal.Event{{
		Seq: 1, Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}}, reporter.lastHeartbeat.Add(runWaitHeartbeatInterval))

	if got := progress.String(); got != "" {
		t.Fatalf("progress after run.finished = %q, want no heartbeat", got)
	}
}

func waitForProgress(t *testing.T, progress *synchronizedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * runPollInterval)
	for time.Now().Before(deadline) {
		if strings.Contains(progress.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("progress did not contain %q within a poll interval window: %q", want, progress.String())
}

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
