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
	mu            sync.Mutex
	phase         string
	target        string
	started       time.Time
	budgetStarted time.Time
	budgetElapsed time.Duration
	budgetFloor   time.Duration
	accumulation  startupAccumulation
}

func newStartupPhaseTracker(budgetFloor time.Duration) *startupPhaseTracker {
	tracker := &startupPhaseTracker{}
	tracker.configureBudget(budgetFloor)
	return tracker
}

func (t *startupPhaseTracker) set(phase, target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase, t.target, t.started = phase, target, time.Now()
}

func (t *startupPhaseTracker) update(phase, target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase == phase {
		t.target = target
	}
}

func (t *startupPhaseTracker) clear(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase == phase {
		t.phase, t.target, t.started = "", "", time.Time{}
	}
}

func (t *startupPhaseTracker) snapshot() (phase, target string, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase, t.target, t.started
}

// syncWriter serializes concurrent writers onto one underlying io.Writer.
//
// runUpContextWithForce starts watchStartupReadiness in its own goroutine
// (#4368) while the main goroutine keeps running runStartupPhase for each
// subsequent synchronous startup phase (#4570) — both write diagnostics to
// the same stdout with no synchronization otherwise. A bare io.Writer gives
// no such guarantee: concurrent Write calls on it can interleave mid-line, or
// race outright when the underlying writer is not itself concurrency-safe
// (e.g. bytes.Buffer in tests). Wrapping the daemon's stdout once, before the
// watchdog goroutine starts, serializes every writer that goes through it —
// not just those two — with one mutex rather than one per call site.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// syncStartupStdout wraps runUpContextWithForce's stdout in a syncWriter
// (#4570). watchStartupReadiness runs in its own goroutine, started before
// the synchronous startup phases, while the main goroutine keeps writing
// startup-phase diagnostics to the same stdout — with no synchronization
// between them otherwise. Called once, before that goroutine starts, this
// serializes every writer that goes through the returned value for the rest
// of the caller's function.
func syncStartupStdout(stdout io.Writer) io.Writer {
	return &syncWriter{w: stdout}
}

type startupBudgetSnapshot struct {
	Accumulation startupAccumulation
	Budget       time.Duration
	Elapsed      time.Duration
	State        string
	UsedPercent  float64
}

func (t *startupPhaseTracker) configureBudget(floor time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.budgetFloor = floor
	if t.budgetStarted.IsZero() {
		t.budgetStarted = time.Now()
	}
}

func (t *startupPhaseTracker) setWorktreeAccumulation(count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accumulation.Worktrees = count
}

func (t *startupPhaseTracker) setRecoveryAccumulation(count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accumulation.RecoveryRuns = count
}

func (t *startupPhaseTracker) completeBudget(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.budgetStarted.IsZero() && t.budgetElapsed == 0 {
		t.budgetElapsed = now.Sub(t.budgetStarted)
	}
}

func (t *startupPhaseTracker) budgetSnapshot(now time.Time) startupBudgetSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	budget := deriveStartupBudget(t.budgetFloor, t.accumulation)
	var elapsed time.Duration
	if t.budgetElapsed > 0 {
		elapsed = t.budgetElapsed
	} else if !t.budgetStarted.IsZero() {
		elapsed = now.Sub(t.budgetStarted)
	}
	if elapsed < 0 {
		elapsed = 0
	}
	state, used := startupBudgetState(elapsed, budget)
	return startupBudgetSnapshot{
		Accumulation: t.accumulation,
		Budget:       budget,
		Elapsed:      elapsed,
		State:        state,
		UsedPercent:  used,
	}
}

// startupTimestamp formats now for a startup log line. A fixed, sortable,
// greppable format (unlike the daemon's ordinary un-timestamped stdout
// lines) so a blocked startup operation can be correlated to wall-clock time
// without enabling secret-bearing tracing (#4368).
func startupTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newSchedulerSetupProgress logs the existing human-readable setup message
// together with total and inter-step elapsed times. The setup builder owns
// the operation boundaries, so timing its progress transitions avoids
// duplicating its initialization sequence in the daemon orchestrator.
func newSchedulerSetupProgress(w io.Writer, started time.Time, now func() time.Time) func(string) {
	previous := started
	return func(message string) {
		current := now()
		pf(
			w,
			"%s startup: %s phase=scheduler-setup status=progress elapsed=%s since-previous=%s\n",
			current.UTC().Format(time.RFC3339Nano),
			message,
			current.Sub(started),
			current.Sub(previous),
		)
		previous = current
	}
}

// runStartupPhase logs the start and completion (or failure) of a bounded,
// potentially blocking startup operation with a timestamp, the operation
// name, a bounded target identity (e.g. a gaggle or repository name), and
// elapsed time (#4368's acceptance criteria), and records it on tracker so a
// concurrent readiness watchdog can name the current phase. target may be
// empty for a phase with no single bounded identity.
func runStartupPhase(w io.Writer, tracker *startupPhaseTracker, phase, target string, fn func() error) error {
	if tracker != nil {
		tracker.set(phase, target)
		defer tracker.clear(phase)
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

// logGateFlip logs a startup readiness gate (configLoaded, stateOpen,
// resumeComplete, sweepsStarted, planeReady, ready) flipping true, with
// elapsed time since processStart (#4252). Before this, each of those gates
// was set with a bare atomic.Bool.Store(true) and no log line at all, so a
// crash-resume that legitimately takes minutes (resumeComplete is unbounded,
// scaling with interrupted-run count) was indistinguishable in the container
// log from an earlier phase silently hanging — nothing recorded WHICH gate
// had flipped, or when, so an operator watching the log during the gap could
// not tell "still resuming, on track" from "stuck".
func logGateFlip(w io.Writer, processStart time.Time, gate string) {
	pf(w, "%s startup gate=%s status=flipped elapsed=%s\n", startupTimestamp(), gate, time.Since(processStart))
}

// watchStartupReadiness emits one diagnostic log line naming the startup
// phase runUpContext is currently in if the daemon is still alive but has
// not reached readiness within its accumulation-derived budget (#4368's
// original acceptance criteria: "if
// startup is alive but has not reached readiness within the health
// threshold, emit a diagnostic identifying the current startup phase rather
// than relying only on a stale heartbeat"). threshold is now only the
// zero-accumulation floor; measured worktrees and recovery runs extend it.
// A non-positive threshold disables the watchdog. Returns once ctx is done
// or the single check has run.
func watchStartupReadiness(ctx context.Context, w io.Writer, tracker *startupPhaseTracker, ready func() bool, threshold time.Duration) {
	if threshold <= 0 {
		return
	}
	tracker.configureBudget(threshold)
	timer := time.NewTimer(threshold)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ready() {
				return
			}
			budget := tracker.budgetSnapshot(time.Now())
			if budget.Elapsed < budget.Budget {
				timer.Reset(budget.Budget - budget.Elapsed)
				continue
			}
			phase, target, since := tracker.snapshot()
			if phase == "" {
				phase = "unknown"
			}
			pf(w, "%s startup diagnostic: daemon alive but not ready after derived budget %s; accumulation worktrees=%d recovery-runs=%d total=%d; currently in phase=%s target=%q (running for %s)\n",
				startupTimestamp(), budget.Budget, budget.Accumulation.Worktrees, budget.Accumulation.RecoveryRuns,
				budget.Accumulation.total(), phase, target, time.Since(since))
			return
		}
	}
}
