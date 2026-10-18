package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// settleStalledEngineRun is the stall sweep's arm for an engine-driven run
// that is past its stall timeout or maximum duration.
//
// The first answer is always a CancelWorkflow request, and the journal is left
// alone: the engine's cancel arm writes the run's terminal once a worker picks
// the cancellation up. A request is not an outcome, so it never releases the
// run's scheduler slot.
//
// A worker that cannot poll never picks it up (#5407). Once a full stall
// timeout has passed since the sweep's first recorded cancellation, with the
// journal still silent, the sweep stops asking: it terminates the workflow on
// the server, which needs no worker, and only after Temporal confirms the
// workflow is closed does it write the run's terminal through the live journal
// plane and release the slot. The cancellation is dated by the sweep's own
// instance-log annotation, so a daemon restart neither resets nor skips the
// wait, and a run first seen long after it went silent is still cancelled
// before it can be terminated.
func settleStalledEngineRun(
	ctx context.Context,
	guards *engineRunGuards,
	log *journal.InstanceLog,
	deps *stalledSweepDeps,
	cancels *engineCancelHistory,
	release func(runID, workflow string),
	identity journal.RunIdentity,
	events []journal.Event,
	now time.Time,
	suspended []hostsuspend.Window,
	runTimeout, runMaxDuration time.Duration,
	durationExceeded bool,
) error {
	message := fmt.Sprintf("run exceeded %s without journal activity", runTimeout)
	// The interruption's code travels with the sweep's own record: the
	// engine can journal a cancellation only as run_canceled, and the failure
	// streak needs to tell this watchdog stop from an operator's (#5638).
	interrupt := runner.RunStalledErrorCode
	if durationExceeded {
		message = fmt.Sprintf("run exceeded maximum duration %s", runMaxDuration)
		interrupt = runner.RunDurationExceededErrorCode
	}
	unhonoured, err := engineCancelUnhonoured(deps, cancels, identity.RunID, events, now, suspended, runTimeout)
	if err != nil {
		return fmt.Errorf("read engine cancellations for run %q: %w", identity.RunID, err)
	}
	if !unhonoured {
		// Deliberately no `release`: the run's scheduler slot belongs to
		// whoever learns the outcome — reattachEngineRun's goroutine for a
		// run this daemon seeded at startup — and freeing it on a request
		// that has not landed is a duplicate-admission hazard.
		//
		// The record is written BEFORE the cancel (#5638): the engine's
		// terminal hooks read it to tell this watchdog stop from an
		// operator's, and they can run as soon as the cancellation lands. A
		// record whose cancel then fails is harmless — no run_canceled
		// terminal follows from it, and an operator's own cancel still wins.
		if err := appendEngineRecovery(log, identity, message, journal.RecoveryActionEngineCancelRequested, interrupt); err != nil {
			return fmt.Errorf("journal engine cancel for run %q: %w", identity.RunID, err)
		}
		if err := guards.cancel(ctx, identity.RunID); err != nil {
			return fmt.Errorf("cancel stalled engine run %q: %w", identity.RunID, err)
		}
		return nil
	}
	cause := fmt.Sprintf("%s; the engine did not act on the cancellation for a further %s, so the daemon terminated its workflow",
		message, runTimeout)
	terminated, err := guards.terminateUnresponsive(ctx, identity.RunID, cause)
	if err != nil {
		return fmt.Errorf("terminate stalled engine run %q: %w", identity.RunID, err)
	}
	if !terminated {
		// Closed some other way: the outcome is in the workflow's history,
		// and the completed-run reconciler records it; a daemon-written
		// terminal would overwrite it with a termination that never happened.
		return nil
	}
	closeRun := deps.closeEngineRun()
	if closeRun == nil {
		return fmt.Errorf("close terminated engine run %q: this daemon has no live journal writer", identity.RunID)
	}
	if err := closeRun(ctx, identity, cause, now); err != nil {
		return fmt.Errorf("close terminated engine run %q: %w", identity.RunID, err)
	}
	// The startup sweep runs before crash-resume, which does not reattach a
	// closed journal, so the slot is released here rather than left to an
	// owner that may never exist. A later owner release is a no-op.
	if release != nil {
		release(identity.RunID, identity.Workflow)
	}
	if err := appendEngineRecovery(log, identity, cause, journal.RecoveryActionEngineTerminated, interrupt); err != nil {
		return fmt.Errorf("journal engine terminate for run %q: %w", identity.RunID, err)
	}
	return nil
}

// engineCancelUnhonoured reports whether the sweep asked the engine to cancel
// a stalled run at least one full stall timeout ago, after the run's last
// journal activity, and the journal has stayed silent since: had any worker
// taken the cancellation, the engine's cancel arm would have journaled the
// run's terminal by now. Graceful-drain downtime and host suspension (#5891)
// are credited exactly as they are for the stall itself. The cancellation is dated on its own, never from the
// stall: a maximum-duration breach cancels a run that was active moments
// before. The instance log is consulted only once the journal has been silent
// for a full timeout, the least a cancellation after it could have waited.
func engineCancelUnhonoured(
	deps *stalledSweepDeps,
	cancels *engineCancelHistory,
	runID string,
	events []journal.Event,
	now time.Time,
	suspended []hostsuspend.Window,
	runTimeout time.Duration,
) (bool, error) {
	if runTimeout <= 0 || len(events) == 0 {
		return false, nil
	}
	lastActivity := events[len(events)-1].Time
	if lastActivity.IsZero() || !lastActivity.Before(now.Add(-runTimeout)) {
		return false, nil
	}
	requested, ok, err := cancels.firstRequestAfter(runID, lastActivity)
	if err != nil || !ok {
		return false, err
	}
	credit := deps.drainedDowntimeSince(requested) + hostsuspend.Overlap(suspended, requested, now)
	return requested.Before(now.Add(-(runTimeout + credit))), nil
}

// engineCancelHistory dates the stall sweep's own cancellation requests from
// their engine_cancel_requested annotations in the instance log, which a
// daemon restart keeps. One sweep's view refreshes the shared per-journal
// index at most once.
type engineCancelHistory struct {
	log       *journal.InstanceLog
	index     *engineCancelIndex
	refreshed bool
}

// engineCancelIndex is the cancellation index the stall sweeps of one
// instance journal share. It is folded incrementally, so a sweep parses only
// what was appended since the previous one rather than the whole journal
// (#7031).
type engineCancelIndex struct {
	mu       sync.Mutex
	cursor   *journal.InstanceLogCursor
	requests map[string][]time.Time
	// interrupts is the latest watchdog interruption the sweep recorded for
	// each run it cancelled; operatorCancels marks runs an operator also
	// asked to cancel (#5638).
	interrupts      map[string]string
	operatorCancels map[string]bool
}

var engineCancelIndexes sync.Map // scheduler directory -> *engineCancelIndex

func (h *engineCancelHistory) firstRequestAfter(runID string, after time.Time) (time.Time, bool, error) {
	if h == nil || h.log == nil {
		return time.Time{}, false, nil
	}
	if h.index == nil {
		dir := h.log.Dir()
		value, _ := engineCancelIndexes.LoadOrStore(dir, &engineCancelIndex{cursor: journal.NewInstanceLogCursor(dir)})
		h.index = value.(*engineCancelIndex)
	}
	h.index.mu.Lock()
	defer h.index.mu.Unlock()
	if !h.refreshed {
		if err := h.index.refresh(); err != nil {
			return time.Time{}, false, err
		}
		h.refreshed = true
	}
	for _, at := range h.index.requests[runID] {
		if at.After(after) {
			return at, true, nil
		}
	}
	return time.Time{}, false, nil
}

func (x *engineCancelIndex) refresh() error {
	events, reset, err := x.cursor.Next()
	if err != nil {
		return err
	}
	if reset || x.requests == nil {
		x.requests = make(map[string][]time.Time)
		x.interrupts = make(map[string]string)
		x.operatorCancels = make(map[string]bool)
	}
	for _, ev := range events {
		if ev.Type != journal.EventRunnerAnnotation || ev.RunID == "" {
			continue
		}
		if action, _ := ev.Runner["action"].(string); action == journal.RecoveryActionEngineCancelRequested {
			x.requests[ev.RunID] = append(x.requests[ev.RunID], ev.Time)
			if interrupt, _ := ev.Runner[engineRecoveryInterruptKey].(string); interrupt != "" {
				x.interrupts[ev.RunID] = interrupt
			} else {
				x.operatorCancels[ev.RunID] = true
			}
		}
	}
	return nil
}

// engineRecoveryInterruptKey names, on the stall sweep's own recovery
// annotations, the watchdog interruption that made it stop the run
// (runner.RunStalledErrorCode or RunDurationExceededErrorCode). An operator's
// cancellation annotation never carries it.
const engineRecoveryInterruptKey = "interrupt"

func appendEngineRecovery(log *journal.InstanceLog, identity journal.RunIdentity, reason, action, interrupt string) error {
	if log == nil {
		return nil
	}
	return log.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Gaggle: identity.Gaggle, Workflow: identity.Workflow, RunID: identity.RunID,
		Runner: map[string]any{
			"kind":                     journal.RunnerAnnotationRunRecovery,
			"reason":                   reason,
			"action":                   action,
			"driver":                   string(identity.Driver),
			engineRecoveryInterruptKey: interrupt,
		},
	})
}

// engineWatchdogInterruption is escalationnotify.Policy.EngineInterruption:
// the watchdog interruption the stall sweep recorded when it asked the engine
// to cancel runID, or "" when it never did — or when an operator also asked,
// because the engine journals both the same way and an operator's cancel must
// never be read as infrastructure (#5638).
//
// "Operator wins" depends on the operator's own engine_cancel_requested
// annotation reaching the instance log. The CLI path appends it reporting any
// failure; the daemon's cancel plane appends it best-effort. If that append is
// lost while a sweep record exists for the same run, the operator's cancel is
// read as the sweep's interruption — a rare residual, accepted because the
// alternative (ignoring the sweep's record) would charge every engine
// watchdog stop as work.
func engineWatchdogInterruption(schedulerDir, runID string) (string, error) {
	value, _ := engineCancelIndexes.LoadOrStore(schedulerDir, &engineCancelIndex{cursor: journal.NewInstanceLogCursor(schedulerDir)})
	index := value.(*engineCancelIndex)
	index.mu.Lock()
	defer index.mu.Unlock()
	if err := index.refresh(); err != nil {
		return "", err
	}
	if index.operatorCancels[runID] {
		return "", nil
	}
	return index.interrupts[runID], nil
}

// closeTerminatedEngineRun returns the stall sweep's CloseEngineRun over the
// daemon's live journal writer, or nil when the daemon has none. A journal
// that is already terminal (the engine's own terminal landed first) is
// closed, which is all the sweep needs.
func closeTerminatedEngineRun(live *livejournal.Writer) func(context.Context, journal.RunIdentity, string, time.Time) error {
	if live == nil {
		return nil
	}
	return func(ctx context.Context, identity journal.RunIdentity, cause string, at time.Time) error {
		_, err := live.Emit(ctx, engine.TerminatedRunClosure(identity.RunID, identity.Gaggle, at, cause))
		if errors.Is(err, livejournal.ErrTerminal) {
			return nil
		}
		return err
	}
}
