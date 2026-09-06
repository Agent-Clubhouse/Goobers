package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/daemonlog"
)

// startupPhaseTracker records which startup phase runUpContext is currently
// executing, and since when, so a stuck-but-alive daemon can be diagnosed
// without a debugger or process-tree inspection (#4368). A stale scheduler
// heartbeat only exists once the scheduler is already running, which is too
// late to explain a daemon that never got that far.
type startupPhaseTracker struct {
	mu      sync.Mutex
	phase   string
	target  string
	started time.Time
}

func (t *startupPhaseTracker) set(phase, target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase, t.target, t.started = phase, target, time.Now()
}

func (t *startupPhaseTracker) snapshot() (phase, target string, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase, t.target, t.started
}

// startupTimestamp formats now for a startup log line. A fixed, sortable,
// greppable format (unlike the daemon's ordinary un-timestamped stdout
// lines) so a blocked startup operation can be correlated to wall-clock time
// without enabling secret-bearing tracing (#4368).
func startupTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// runStartupPhase logs the start and completion (or failure) of a bounded,
// potentially blocking startup operation with a timestamp, the operation
// name, a bounded target identity (e.g. a gaggle or repository name), and
// elapsed time (#4368's acceptance criteria), and records it on tracker so a
// concurrent readiness watchdog can name the current phase. target may be
// empty for a phase with no single bounded identity.
func runStartupPhase(w io.Writer, tracker *startupPhaseTracker, phase, target string, fn func() error) error {
	if tracker != nil {
		tracker.set(phase, target)
	}
	start := time.Now()
	pf(w, "%s startup phase=%s status=start target=%q\n", startupTimestamp(), phase, target)
	err := fn()
	elapsed := time.Since(start)
	if err != nil {
		pf(w, "%s startup phase=%s status=failed target=%q elapsed=%s error=%q\n", startupTimestamp(), phase, target, elapsed, daemonlog.Redact(err.Error()))
		return err
	}
	pf(w, "%s startup phase=%s status=done target=%q elapsed=%s\n", startupTimestamp(), phase, target, elapsed)
	return nil
}

// watchStartupReadiness emits one diagnostic log line naming the startup
// phase runUpContext is currently in if the daemon is still alive but has
// not reached readiness within threshold (#4368's acceptance criteria: "if
// startup is alive but has not reached readiness within the health
// threshold, emit a diagnostic identifying the current startup phase rather
// than relying only on a stale heartbeat"). threshold <= 0 disables the
// watchdog. Returns once ctx is done or the single check has run.
func watchStartupReadiness(ctx context.Context, w io.Writer, tracker *startupPhaseTracker, ready func() bool, threshold time.Duration) {
	if threshold <= 0 {
		return
	}
	timer := time.NewTimer(threshold)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		if ready() {
			return
		}
		phase, target, since := tracker.snapshot()
		if phase == "" {
			phase = "unknown"
		}
		pf(w, "%s startup diagnostic: daemon alive but not ready after %s; currently in phase=%s target=%q (running for %s)\n",
			startupTimestamp(), threshold, phase, target, time.Since(since))
	}
}
