package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// RunStalledErrorCode identifies watchdog terminalizations in run journals.
const RunStalledErrorCode = "run_stalled"

// RunCanceledErrorCode identifies operator-requested live cancellations
// (`goobers run cancel`, #831) in run journals, distinct from the watchdog's
// stall terminalizations (RunStalledErrorCode).
const RunCanceledErrorCode = "run_canceled"

// RunDurationExceededErrorCode identifies watchdog cancellations caused by a
// run exceeding its configured total wall-clock duration.
const RunDurationExceededErrorCode = "run_duration_exceeded"

// StageInterruptedErrorCode identifies operator-requested interrupts of a
// single running agentic stage (#1995) in run journals. Unlike a cancel
// (RunCanceledErrorCode), which terminalizes the whole run aborted, a stage
// interrupt terminalizes the run escalated — a recoverable phase from which
// RerunStage continues the interrupted stage with an operator correction
// folded into the envelope. It is distinct from the watchdog's stall
// terminalization (RunStalledErrorCode): the trigger is a human, not the
// no-progress watchdog.
const StageInterruptedErrorCode = "stage_interrupted"

var (
	errStalledRun          = errors.New("runner: stalled run escalation requested")
	errCanceledRun         = errors.New("runner: run cancellation requested")
	errRunDurationExceeded = errors.New("runner: maximum run duration exceeded")
	errHardShutdown        = errors.New("runner: hard shutdown requested")
	errInterruptedStage    = errors.New("runner: stage interruption requested")
)

// interruptKind distinguishes why a live run's active attempt was interrupted,
// which selects the terminal phase and journal note the owner records when it
// unwinds. All kinds share the one activeRun handshake (withActiveRun) so
// watchdog and manual interrupts cannot race into two terminalizations.
type interruptKind int

const (
	interruptStalled interruptKind = iota
	interruptCancel
	interruptDurationExceeded
	interruptHardShutdown
	// interruptStage is an operator interrupt of a single running stage
	// (#1995). Like interruptStalled it terminalizes the run escalated
	// (recoverable), but the trigger is a human and the note it records is
	// operator-attributed, distinguishing it from a watchdog stall.
	interruptStage
)

// stalledRequest is a pending interrupt of a live run's active attempt. now is
// always set; timeout/lastActivity carry watchdog diagnostics and are zero for
// an operator cancel. phase and cause drive terminalization and cancellation.
type stalledRequest struct {
	kind         interruptKind
	now          time.Time
	timeout      time.Duration
	lastActivity time.Time
	phase        journal.RunPhase
	cause        error
	// stage and actor are set only for an operator stage interrupt
	// (interruptStage): the targeted stage and the human principal who
	// requested it, recorded on the interrupt note for attribution.
	stage string
	actor string
}

type activeRunResult struct {
	result Result
	err    error
}

type takeoverClaim int

const (
	takeoverClaimed takeoverClaim = iota
	takeoverReady
	takeoverOwnerTerminalizing
	takeoverAlreadyClaimed
)

type activeRun struct {
	attemptCtx   context.Context
	cancel       context.CancelCauseFunc
	done         chan struct{}
	ownerDone    chan struct{}
	takeoverDone chan struct{}
	journal      *journal.Run

	mu                 sync.Mutex
	request            *stalledRequest
	ownerFinished      bool
	ownerOutcome       activeRunResult
	ownerTerminalizing bool
	takenOver          bool
	takeoverOutcome    activeRunResult
	completed          bool
	outcome            activeRunResult
}

type activeRunSet struct {
	mu        sync.Mutex
	runs      map[string]*activeRun
	hardStops map[string]struct{}
}

type activeRunContextKey struct{}

func (r *Runner) withActiveRun(ctx context.Context, runID string, jr *journal.Run, run func(context.Context) (Result, error)) (Result, error) {
	return r.withActiveRunCleanup(ctx, runID, jr, run, nil)
}

func (r *Runner) withActiveRunCleanup(ctx context.Context, runID string, jr *journal.Run, run func(context.Context) (Result, error), ownerCleanup func() error) (Result, error) {
	attemptCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	active := &activeRun{
		attemptCtx:   attemptCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
		ownerDone:    make(chan struct{}),
		takeoverDone: make(chan struct{}),
		journal:      jr,
	}

	r.active.mu.Lock()
	if r.active.runs == nil {
		r.active.runs = make(map[string]*activeRun)
	}
	if _, exists := r.active.runs[runID]; exists {
		r.active.mu.Unlock()
		cancel(nil)
		err := fmt.Errorf("runner: run %q already has an active owner", runID)
		if ownerCleanup != nil {
			err = errors.Join(err, ownerCleanup())
		}
		return Result{}, err
	}
	r.active.runs[runID] = active
	_, hardStop := r.active.hardStops[runID]
	delete(r.active.hardStops, runID)
	r.active.mu.Unlock()
	if hardStop {
		active.requestHardShutdown()
	}

	// Keep the public Start/Resume owner outside the workflow goroutine so a
	// watchdog takeover can return even if an invocation ignores cancellation.
	ownedCtx := context.WithValue(ctx, activeRunContextKey{}, active)
	go func() {
		result, err := run(ownedCtx)
		if ownerCleanup != nil {
			err = errors.Join(err, ownerCleanup())
		}
		active.mu.Lock()
		active.ownerFinished = true
		active.ownerOutcome = activeRunResult{result: result, err: err}
		close(active.ownerDone)
		active.mu.Unlock()
	}()

	var outcome activeRunResult
	select {
	case <-active.ownerDone:
		active.mu.Lock()
		takenOver := active.takenOver
		outcome = active.ownerOutcome
		active.mu.Unlock()
		if takenOver {
			<-active.takeoverDone
			active.mu.Lock()
			outcome = active.takeoverOutcome
			active.mu.Unlock()
		}
	case <-active.takeoverDone:
		active.mu.Lock()
		outcome = active.takeoverOutcome
		active.mu.Unlock()
	}

	r.active.mu.Lock()
	delete(r.active.runs, runID)
	active.mu.Lock()
	active.completed = true
	active.outcome = outcome
	cancel = active.cancel
	close(active.done)
	active.mu.Unlock()
	r.active.mu.Unlock()
	cancel(nil)
	return outcome.result, outcome.err
}

func (r *Runner) activeRun(runID string) *activeRun {
	r.active.mu.Lock()
	defer r.active.mu.Unlock()
	return r.active.runs[runID]
}

func (r *Runner) claimOwnerTerminalization(runID string) (activeRunResult, bool) {
	active := r.activeRun(runID)
	if active == nil {
		return activeRunResult{}, false
	}
	return active.claimOwnerTerminalization()
}

func (r *activeRun) requestEscalation(now time.Time, timeout time.Duration) (requested, refreshed bool) {
	stale := r.journal.IfLastActivityBefore(now.Add(-timeout), func(lastActivity time.Time) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.completed {
			return
		}
		if r.request == nil {
			r.request = &stalledRequest{
				kind:         interruptStalled,
				now:          now,
				timeout:      timeout,
				lastActivity: lastActivity,
				phase:        journal.PhaseEscalated,
				cause:        errStalledRun,
			}
			r.cancel(errStalledRun)
		}
		requested = true
	})
	if !stale {
		return false, true
	}
	return requested, false
}

// requestInterrupt unconditionally interrupts a live run unless it completed
// or another watchdog/operator request already owns the terminalization slot.
func (r *activeRun) requestInterrupt(request stalledRequest) (requested bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completed {
		return false
	}
	if r.request == nil {
		r.request = &request
		r.cancel(request.cause)
	}
	return r.request.kind == request.kind
}

func (r *activeRun) requestCancel(now time.Time) (requested bool) {
	return r.requestInterrupt(stalledRequest{
		kind:  interruptCancel,
		now:   now,
		phase: journal.PhaseAborted,
		cause: errCanceledRun,
	})
}

func (r *activeRun) requestHardShutdown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completed {
		return false
	}
	request := stalledRequest{
		kind:  interruptHardShutdown,
		cause: errHardShutdown,
	}
	r.request = &request
	r.cancel(request.cause)
	return true
}

// HardStopRun interrupts an owned in-flight stage without terminalizing its
// run. The owner checkpoints the current machine state so startup can resume it.
func (r *Runner) HardStopRun(runID string) bool {
	active := r.activeRun(runID)
	if active == nil {
		return false
	}
	return active.requestHardShutdown()
}

// HardStopRunWhenStarted interrupts runID now or as soon as its active run
// context is registered.
func (r *Runner) HardStopRunWhenStarted(runID string) {
	r.active.mu.Lock()
	active := r.active.runs[runID]
	if active == nil {
		if r.active.hardStops == nil {
			r.active.hardStops = make(map[string]struct{})
		}
		r.active.hardStops[runID] = struct{}{}
		r.active.mu.Unlock()
		return
	}
	r.active.mu.Unlock()
	active.requestHardShutdown()
}

func (r *activeRun) waitFor(timeout time.Duration) (activeRunResult, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.outcome, true
	case <-timer.C:
		return activeRunResult{}, false
	}
}

func (r *activeRun) claimTakeover() (activeRunResult, takeoverClaim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.completed:
		return r.outcome, takeoverReady
	case r.ownerFinished:
		return r.ownerOutcome, takeoverReady
	case r.ownerTerminalizing:
		return activeRunResult{}, takeoverOwnerTerminalizing
	case r.takenOver:
		select {
		case <-r.takeoverDone:
			return r.takeoverOutcome, takeoverReady
		default:
			return activeRunResult{}, takeoverAlreadyClaimed
		}
	default:
		r.takenOver = true
		return activeRunResult{}, takeoverClaimed
	}
}

func (r *activeRun) claimOwnerTerminalization() (activeRunResult, bool) {
	r.mu.Lock()
	if !r.takenOver {
		r.ownerTerminalizing = true
		r.mu.Unlock()
		return activeRunResult{}, false
	}
	takeoverDone := r.takeoverDone
	r.mu.Unlock()

	<-takeoverDone
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.takeoverOutcome, true
}

func (r *activeRun) completeTakeover(outcome activeRunResult) {
	r.mu.Lock()
	r.takeoverOutcome = outcome
	close(r.takeoverDone)
	r.mu.Unlock()
}

func stalledRequestFromContext(ctx context.Context) (stalledRequest, bool) {
	active, ok := ctx.Value(activeRunContextKey{}).(*activeRun)
	if !ok {
		return stalledRequest{}, false
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.request == nil {
		return stalledRequest{}, false
	}
	return *active.request, true
}

func stalledAttemptContext(ctx context.Context) context.Context {
	active, ok := ctx.Value(activeRunContextKey{}).(*activeRun)
	if !ok {
		return context.WithoutCancel(ctx)
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	return active.attemptCtx
}

func setStalledAttemptContext(ctx context.Context) {
	active, ok := ctx.Value(activeRunContextKey{}).(*activeRun)
	if !ok {
		return
	}
	attemptCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	active.mu.Lock()
	previousCancel := active.cancel
	active.attemptCtx = attemptCtx
	active.cancel = cancel
	var cause error
	if active.request != nil {
		cause = active.request.cause
	}
	active.mu.Unlock()

	previousCancel(nil)
	if cause != nil {
		cancel(cause)
	}
}

// newestTimestamped returns the time of the most recent event carrying a
// non-zero timestamp, or the zero time when none does. Callers treat that zero
// as "cannot determine", never as "idle forever".
func newestTimestamped(events []journal.Event) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if !events[i].Time.IsZero() {
			return events[i].Time
		}
	}
	return time.Time{}
}

type stalledCandidate struct {
	phase        journal.RunPhase
	lastActivity time.Time
	finalState   string
}

func inspectStalledCandidate(dir, runID string, now time.Time, timeout time.Duration) (stalledCandidate, bool, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return stalledCandidate{}, false, fmt.Errorf("runner: open stalled run %q: %w", runID, err)
	}
	phase, err := reader.Phase()
	if err != nil {
		return stalledCandidate{}, false, fmt.Errorf("runner: reconstruct stalled run %q phase: %w", runID, err)
	}
	candidate := stalledCandidate{phase: phase}
	if phase != journal.PhaseRunning {
		return candidate, false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return stalledCandidate{}, false, fmt.Errorf("runner: read stalled run %q events: %w", runID, err)
	}
	if len(events) == 0 {
		return stalledCandidate{}, false, fmt.Errorf("runner: running run %q has no journal events", runID)
	}
	if events[len(events)-1].Type == journal.EventGatePaused {
		return candidate, false, nil
	}
	// Take the newest event that actually carries a TIMESTAMP. An event written
	// without one says nothing about when activity happened, and reading its
	// zero Time as "last active at year 1" makes every subsequent comparison
	// trivially true — the run is then escalated no matter how large the
	// timeout is, which is the tell that no configuration can fix it.
	//
	// MEASURED (#3774): run 8238995d was escalated 11 minutes into a 60-minute
	// agentic stage. Its newest event was agent.lifecycle, written with
	// time 0001-01-01T00:00:00Z, so lastActivity was zero and the message read
	// "no journal progress for 2562047h47m16s" — while four correctly stamped
	// events sat directly above it. The stamp is missing at the writer, which is
	// its own defect; this is the reader refusing to draw a conclusion the data
	// does not support.
	//
	// The practical effect: an agentic stage emits agent.lifecycle and then
	// works silently, so ANY run whose newest event is that one was killed on
	// the next sweep. Long agentic work could not complete in mode 3 at all.
	candidate.lastActivity = newestTimestamped(events)
	if candidate.lastActivity.IsZero() {
		return candidate, false, nil
	}
	if !candidate.lastActivity.Before(now.Add(-timeout)) {
		return candidate, false, nil
	}
	if state, stateErr := reader.State(); stateErr == nil {
		candidate.finalState = state.MachineState
	}
	return candidate, true, nil
}

// EscalateStalled rechecks a candidate and, if it is still running and silent
// past timeout, asks its live owner to stop the active attempt. Runs without a
// live owner are recovered and finished directly.
func (r *Runner) EscalateStalled(runID string, now time.Time, timeout time.Duration) (Result, bool, error) {
	if !apiv1.ValidRunID(runID) {
		return Result{}, false, fmt.Errorf("runner: invalid run id %q", runID)
	}
	if timeout <= 0 {
		return Result{}, false, fmt.Errorf("runner: stalled run timeout must be positive, got %s", timeout)
	}

	dir := filepath.Join(r.cfg.RunsDir, runID)
	if active := r.activeRun(runID); active != nil {
		candidate, stalled, err := inspectStalledCandidate(dir, runID, now, timeout)
		if err != nil {
			return Result{}, false, err
		}
		if !stalled {
			return Result{Phase: candidate.phase}, false, nil
		}
		requested, refreshed := active.requestEscalation(now, timeout)
		if refreshed {
			return Result{Phase: journal.PhaseRunning}, false, nil
		}
		if requested {
			grace := r.stalledCancelGrace
			if grace <= 0 {
				grace = StalledCancellationGrace
			}
			if outcome, ok := active.waitFor(grace); ok {
				return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
			}
			outcome, claim := active.claimTakeover()
			switch claim {
			case takeoverReady:
				return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
			case takeoverOwnerTerminalizing, takeoverAlreadyClaimed:
				terminalGrace := r.stalledTerminalGrace
				if terminalGrace <= 0 {
					terminalGrace = StalledTerminalizationGrace
				}
				if outcome, ok := active.waitFor(terminalGrace); ok {
					return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
				}
				return Result{}, false, fmt.Errorf("runner: stalled run %q did not finish terminalization within %s", runID, grace+terminalGrace)
			}
			result, finishErr := r.finishStalledTakeover(runID, active.journal, candidate.finalState, 0, stalledRequest{
				kind:         interruptStalled,
				now:          now,
				timeout:      timeout,
				lastActivity: candidate.lastActivity,
				phase:        journal.PhaseEscalated,
				cause:        errStalledRun,
			})
			active.completeTakeover(activeRunResult{result: result, err: finishErr})
			return result, result.Phase == journal.PhaseEscalated, finishErr
		}
	}

	_, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: recover stalled run %q: %w", runID, err)
	}
	defer func() { _ = jr.Close() }()

	candidate, stalled, err := inspectStalledCandidate(dir, runID, now, timeout)
	if err != nil {
		return Result{}, false, err
	}
	if !stalled {
		return Result{Phase: candidate.phase}, false, nil
	}
	result, err := r.finishStalled(runID, jr, candidate.finalState, 0, stalledRequest{
		kind:         interruptStalled,
		now:          now,
		timeout:      timeout,
		lastActivity: candidate.lastActivity,
		phase:        journal.PhaseEscalated,
		cause:        errStalledRun,
	})
	return result, result.Phase == journal.PhaseEscalated, err
}

// CancelRun stops a specific in-flight run this Runner owns at operator request
// (`goobers run cancel`, #831): it interrupts the run's active attempt, lets the
// owner unwind, and finalizes the run terminal phase aborted — tearing down the
// worktree and releasing the backlog claim through the same terminal path a
// stall escalation or a normal finish uses (Config.FinalizeTerminal). It routes
// through the same activeRun handshake as EscalateStalled, so a manual cancel
// and the stall watchdog arbitrate to a single terminalization rather than
// racing.
//
// The returned bool reports whether this call cancelled a live run:
//   - false, nil error, no live owner here (r.activeRun(runID) == nil) — the
//     caller distinguishes "not running by this daemon" from a real cancel;
//   - false, nil error, run already terminal (Result.Phase carries the phase);
//   - true — the run reached a terminal phase because of this cancel (aborted,
//     or escalated if a stall interrupt had already claimed the request slot).
func (r *Runner) CancelRun(runID string, now time.Time) (Result, bool, error) {
	if !apiv1.ValidRunID(runID) {
		return Result{}, false, fmt.Errorf("runner: invalid run id %q", runID)
	}

	active := r.activeRun(runID)
	if active == nil {
		// No live attempt here: either this daemon does not own the run, or it
		// finished between the request and this call. Not our run to cancel.
		return Result{}, false, nil
	}

	dir := filepath.Join(r.cfg.RunsDir, runID)
	phase, finalState, err := runPhaseAndState(dir)
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: inspect run %q for cancel: %w", runID, err)
	}
	if phase != journal.PhaseRunning {
		// Terminalized between resolving the owner and here — report the phase,
		// nothing to cancel.
		return Result{Phase: phase}, false, nil
	}

	active.requestCancel(now)

	grace := r.stalledCancelGrace
	if grace <= 0 {
		grace = StalledCancellationGrace
	}
	if outcome, ok := active.waitFor(grace); ok {
		return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
	}
	outcome, claim := active.claimTakeover()
	switch claim {
	case takeoverReady:
		return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
	case takeoverOwnerTerminalizing, takeoverAlreadyClaimed:
		terminalGrace := r.stalledTerminalGrace
		if terminalGrace <= 0 {
			terminalGrace = StalledTerminalizationGrace
		}
		if outcome, ok := active.waitFor(terminalGrace); ok {
			return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
		}
		return Result{}, false, fmt.Errorf("runner: cancelled run %q did not finish terminalization within %s", runID, grace+terminalGrace)
	}
	result, finishErr := r.finishStalledTakeover(runID, active.journal, finalState, 0, stalledRequest{
		kind:  interruptCancel,
		now:   now,
		phase: journal.PhaseAborted,
		cause: errCanceledRun,
	})
	active.completeTakeover(activeRunResult{result: result, err: finishErr})
	return result, result.Phase == journal.PhaseAborted, finishErr
}

// InterruptStage stops the active attempt of a single running stage at operator
// request (#1995) and finalizes the run escalated rather than aborted. Unlike
// CancelRun — which tears the whole run down terminally — an interrupt leaves
// the run in the recoverable escalated phase, records an operator-attributed
// stage_interrupted note, and hands off to RerunStage, which continues the
// interrupted stage with an operator correction folded into its envelope. It
// routes through the same activeRun handshake as CancelRun and the stall
// watchdog, so a manual interrupt and the watchdog arbitrate to a single
// terminalization rather than racing.
//
// stage is the stage the operator targeted; it must be the run's current stage
// (a stale target — e.g. the stage advanced before the request arrived — is
// rejected rather than interrupting whatever is running now). actor is the
// human principal, recorded for attribution.
//
// The returned bool reports whether this call escalated a live run, mirroring
// CancelRun's semantics:
//   - false, nil error, no live owner here (r.activeRun(runID) == nil);
//   - false, nil error, run already terminal (Result.Phase carries the phase);
//   - true — the run reached the escalated phase because of this interrupt.
func (r *Runner) InterruptStage(runID, stage, actor string, now time.Time) (Result, bool, error) {
	if !apiv1.ValidRunID(runID) {
		return Result{}, false, fmt.Errorf("runner: invalid run id %q", runID)
	}
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return Result{}, false, fmt.Errorf("runner: stage is required to interrupt")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return Result{}, false, fmt.Errorf("runner: actor is required to interrupt stage %q", stage)
	}

	active := r.activeRun(runID)
	if active == nil {
		// No live attempt here: either this daemon does not own the run, or it
		// finished between the request and this call. Not our run to interrupt.
		return Result{}, false, nil
	}

	dir := filepath.Join(r.cfg.RunsDir, runID)
	phase, finalState, err := runPhaseAndState(dir)
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: inspect run %q for stage interrupt: %w", runID, err)
	}
	if phase != journal.PhaseRunning {
		// Terminalized between resolving the owner and here — report the phase,
		// nothing to interrupt.
		return Result{Phase: phase}, false, nil
	}
	if finalState != stage {
		// The operator targeted a stage the run has already left (stale portal
		// view). Refuse rather than interrupt a stage they did not mean to.
		return Result{Phase: phase}, false, fmt.Errorf("runner: run %q is at stage %q, not %q; interrupt target is stale", runID, finalState, stage)
	}

	request := stalledRequest{
		kind:  interruptStage,
		now:   now,
		phase: journal.PhaseEscalated,
		cause: errInterruptedStage,
		stage: stage,
		actor: actor,
	}
	active.requestInterrupt(request)

	grace := r.stalledCancelGrace
	if grace <= 0 {
		grace = StalledCancellationGrace
	}
	if outcome, ok := active.waitFor(grace); ok {
		return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
	}
	outcome, claim := active.claimTakeover()
	switch claim {
	case takeoverReady:
		return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
	case takeoverOwnerTerminalizing, takeoverAlreadyClaimed:
		terminalGrace := r.stalledTerminalGrace
		if terminalGrace <= 0 {
			terminalGrace = StalledTerminalizationGrace
		}
		if outcome, ok := active.waitFor(terminalGrace); ok {
			return outcome.result, outcome.result.Phase == journal.PhaseEscalated, outcome.err
		}
		return Result{}, false, fmt.Errorf("runner: interrupted run %q did not finish terminalization within %s", runID, grace+terminalGrace)
	}
	result, finishErr := r.finishStalledTakeover(runID, active.journal, finalState, 0, request)
	active.completeTakeover(activeRunResult{result: result, err: finishErr})
	return result, result.Phase == journal.PhaseEscalated, finishErr
}

// ExpireRun aborts a running journal whose total wall-clock age exceeds
// timeout. It shares CancelRun's active-attempt interruption and terminal path,
// and can recover an unowned journal during daemon startup before resume.
func (r *Runner) ExpireRun(runID string, now, startedAt time.Time, timeout time.Duration) (Result, bool, error) {
	if !apiv1.ValidRunID(runID) {
		return Result{}, false, fmt.Errorf("runner: invalid run id %q", runID)
	}
	if timeout <= 0 {
		return Result{}, false, fmt.Errorf("runner: maximum run duration must be positive, got %s", timeout)
	}

	dir := filepath.Join(r.cfg.RunsDir, runID)
	phase, finalState, err := runPhaseAndState(dir)
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: inspect run %q for duration limit: %w", runID, err)
	}
	if phase != journal.PhaseRunning || !startedAt.Before(now.Add(-timeout)) {
		return Result{Phase: phase}, false, nil
	}

	request := stalledRequest{
		kind:         interruptDurationExceeded,
		now:          now,
		timeout:      timeout,
		lastActivity: startedAt,
		phase:        journal.PhaseAborted,
		cause:        errRunDurationExceeded,
	}
	if active := r.activeRun(runID); active != nil {
		active.requestInterrupt(request)
		grace := r.stalledCancelGrace
		if grace <= 0 {
			grace = StalledCancellationGrace
		}
		if outcome, ok := active.waitFor(grace); ok {
			return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
		}
		outcome, claim := active.claimTakeover()
		switch claim {
		case takeoverReady:
			return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
		case takeoverOwnerTerminalizing, takeoverAlreadyClaimed:
			terminalGrace := r.stalledTerminalGrace
			if terminalGrace <= 0 {
				terminalGrace = StalledTerminalizationGrace
			}
			if outcome, ok := active.waitFor(terminalGrace); ok {
				return outcome.result, outcome.result.Phase == journal.PhaseAborted, outcome.err
			}
			return Result{}, false, fmt.Errorf("runner: expired run %q did not finish terminalization within %s", runID, grace+terminalGrace)
		}
		result, finishErr := r.finishStalledTakeover(runID, active.journal, finalState, 0, request)
		active.completeTakeover(activeRunResult{result: result, err: finishErr})
		return result, result.Phase == journal.PhaseAborted, finishErr
	}

	_, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: recover expired run %q: %w", runID, err)
	}
	defer func() { _ = jr.Close() }()
	result, err := r.finishStalled(runID, jr, finalState, 0, request)
	return result, result.Phase == journal.PhaseAborted, err
}

// runPhaseAndState reads a run's reconstructed terminal phase and its last
// machine state (the stage the run was in) in one open, for the cancel path's
// still-running guard and takeover finalState.
func runPhaseAndState(dir string) (journal.RunPhase, string, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return "", "", err
	}
	phase, err := reader.Phase()
	if err != nil {
		return "", "", err
	}
	finalState := ""
	if state, stateErr := reader.State(); stateErr == nil {
		finalState = state.MachineState
	}
	return phase, finalState, nil
}

func (r *Runner) finishStalled(runID string, jr *journal.Run, finalState string, steps int, request stalledRequest) (Result, error) {
	return r.finishStalledWith(runID, jr, finalState, steps, request, r.finish)
}

func (r *Runner) finishStalledTakeover(runID string, jr *journal.Run, finalState string, steps int, request stalledRequest) (Result, error) {
	return r.finishStalledWith(runID, jr, finalState, steps, request, r.finishTakeover)
}

func (r *Runner) finishStalledWith(
	runID string,
	jr *journal.Run,
	finalState string,
	steps int,
	request stalledRequest,
	finish func(string, *journal.Run, journal.RunPhase, string, int) (Result, error),
) (Result, error) {
	phase := request.phase
	if phase == "" {
		phase = journal.PhaseEscalated
	}
	if err := jr.Append(interruptEvent(runID, request)); err != nil {
		return Result{}, fmt.Errorf("runner: journal interrupted run %q: %w", runID, err)
	}
	return finish(runID, jr, phase, finalState, steps)
}

// interruptEvent is the diagnostic error event recorded before a live run's
// terminal run.finished when a stall or a cancel interrupted its active
// attempt. The two kinds carry distinct codes so run history can tell an
// operator cancellation apart from a watchdog stall terminalization.
func interruptEvent(runID string, request stalledRequest) journal.Event {
	switch request.kind {
	case interruptCancel:
		return journal.Event{
			Type:  journal.EventError,
			Error: &journal.ErrorDetail{Code: RunCanceledErrorCode, Message: fmt.Sprintf("run %q canceled by operator request", runID)},
			Runner: map[string]any{
				"canceledAt": request.now.UTC().Format(time.RFC3339Nano),
			},
		}
	case interruptDurationExceeded:
		return journal.Event{
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    RunDurationExceededErrorCode,
				Message: fmt.Sprintf("run %q exceeded maximum duration %s (started %s)", runID, request.timeout, request.lastActivity.UTC().Format(time.RFC3339)),
			},
			Runner: map[string]any{
				"expiredAt":      request.now.UTC().Format(time.RFC3339Nano),
				"maxRunDuration": request.timeout.String(),
			},
		}
	case interruptStage:
		return journal.Event{
			Type:  journal.EventError,
			Stage: request.stage,
			Actor: request.actor,
			Error: &journal.ErrorDetail{Code: StageInterruptedErrorCode, Message: fmt.Sprintf("run %q stage %q interrupted by operator request", runID, request.stage)},
			Runner: map[string]any{
				"interruptedAt": request.now.UTC().Format(time.RFC3339Nano),
				"stage":         request.stage,
				"actor":         request.actor,
			},
		}
	}
	message := fmt.Sprintf(
		"run %q made no journal progress for %s (last activity %s; timeout %s)",
		runID,
		request.now.Sub(request.lastActivity).Round(time.Second),
		request.lastActivity.UTC().Format(time.RFC3339),
		request.timeout,
	)
	return journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: RunStalledErrorCode, Message: message},
		Runner: map[string]any{
			"lastActivityAt":    request.lastActivity.UTC().Format(time.RFC3339Nano),
			"stalledRunTimeout": request.timeout.String(),
		},
	}
}

func (r *Runner) finishStalledRequest(ctx context.Context, runID string, jr *journal.Run, finalState string, steps int) (Result, bool, error) {
	request, ok := stalledRequestFromContext(ctx)
	if !ok {
		return Result{}, false, nil
	}
	if request.kind == interruptHardShutdown {
		jr.SetMachineState(finalState)
		if err := jr.Checkpoint(); err != nil {
			return Result{}, true, fmt.Errorf("runner: checkpoint hard-stopped run %q at %q: %w", runID, finalState, err)
		}
		return Result{Phase: journal.PhaseRunning, FinalState: finalState, Steps: steps}, true, nil
	}
	active, activeOK := ctx.Value(activeRunContextKey{}).(*activeRun)
	if activeOK {
		if outcome, takenOver := active.claimOwnerTerminalization(); takenOver {
			return outcome.result, true, outcome.err
		}
	}
	result, err := r.finishStalled(runID, jr, finalState, steps, request)
	return result, true, err
}
