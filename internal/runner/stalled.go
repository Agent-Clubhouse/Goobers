package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// RunStalledErrorCode identifies watchdog terminalizations in run journals.
const RunStalledErrorCode = "run_stalled"

var errStalledRun = errors.New("runner: stalled run escalation requested")

type stalledRequest struct {
	now          time.Time
	timeout      time.Duration
	lastActivity time.Time
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
	mu   sync.Mutex
	runs map[string]*activeRun
}

type activeRunContextKey struct{}

func (r *Runner) withActiveRun(ctx context.Context, runID string, jr *journal.Run, run func(context.Context) (Result, error)) (Result, error) {
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
		return Result{}, fmt.Errorf("runner: run %q already has an active owner", runID)
	}
	r.active.runs[runID] = active
	r.active.mu.Unlock()

	// Keep the public Start/Resume owner outside the workflow goroutine so a
	// watchdog takeover can return even if an invocation ignores cancellation.
	ownedCtx := context.WithValue(ctx, activeRunContextKey{}, active)
	go func() {
		result, err := run(ownedCtx)
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
			r.request = &stalledRequest{now: now, timeout: timeout, lastActivity: lastActivity}
			r.cancel(errStalledRun)
		}
		requested = true
	})
	if !stale {
		return false, true
	}
	return requested, false
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
	requested := active.request != nil
	active.mu.Unlock()

	previousCancel(nil)
	if requested {
		cancel(errStalledRun)
	}
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
	candidate.lastActivity = events[len(events)-1].Time
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
				now:          now,
				timeout:      timeout,
				lastActivity: candidate.lastActivity,
			})
			active.completeTakeover(activeRunResult{result: result, err: finishErr})
			return result, result.Phase == journal.PhaseEscalated, finishErr
		}
	}

	_, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
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
		now:          now,
		timeout:      timeout,
		lastActivity: candidate.lastActivity,
	})
	return result, result.Phase == journal.PhaseEscalated, err
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
	message := fmt.Sprintf(
		"run %q made no journal progress for %s (last activity %s; timeout %s)",
		runID,
		request.now.Sub(request.lastActivity).Round(time.Second),
		request.lastActivity.UTC().Format(time.RFC3339),
		request.timeout,
	)
	if err := jr.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: RunStalledErrorCode, Message: message},
		Runner: map[string]any{
			"lastActivityAt":    request.lastActivity.UTC().Format(time.RFC3339Nano),
			"stalledRunTimeout": request.timeout.String(),
		},
	}); err != nil {
		return Result{}, fmt.Errorf("runner: journal stalled run %q: %w", runID, err)
	}
	return finish(runID, jr, journal.PhaseEscalated, finalState, steps)
}

func (r *Runner) finishStalledRequest(ctx context.Context, runID string, jr *journal.Run, finalState string, steps int) (Result, bool, error) {
	request, ok := stalledRequestFromContext(ctx)
	if !ok {
		return Result{}, false, nil
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
