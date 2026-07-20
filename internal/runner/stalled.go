package runner

import (
	"fmt"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// RunStalledErrorCode identifies watchdog terminalizations in run journals.
const RunStalledErrorCode = "run_stalled"

// EscalateStalled rechecks a candidate under the run journal's writer lock and,
// if it is still running and silent past timeout, finishes it through the
// runner's normal escalated terminal path.
func (r *Runner) EscalateStalled(runID string, now time.Time, timeout time.Duration) (Result, bool, error) {
	if !apiv1.ValidRunID(runID) {
		return Result{}, false, fmt.Errorf("runner: invalid run id %q", runID)
	}
	if timeout <= 0 {
		return Result{}, false, fmt.Errorf("runner: stalled run timeout must be positive, got %s", timeout)
	}

	dir := filepath.Join(r.cfg.RunsDir, runID)
	_, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: recover stalled run %q: %w", runID, err)
	}
	defer func() { _ = jr.Close() }()

	reader, err := journal.OpenRead(dir)
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: open stalled run %q: %w", runID, err)
	}
	phase, err := reader.Phase()
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: reconstruct stalled run %q phase: %w", runID, err)
	}
	if phase != journal.PhaseRunning {
		return Result{Phase: phase}, false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return Result{}, false, fmt.Errorf("runner: read stalled run %q events: %w", runID, err)
	}
	if len(events) == 0 {
		return Result{}, false, fmt.Errorf("runner: running run %q has no journal events", runID)
	}
	lastActivity := events[len(events)-1].Time
	if !lastActivity.Before(now.Add(-timeout)) {
		return Result{Phase: journal.PhaseRunning}, false, nil
	}

	finalState := ""
	if state, stateErr := reader.State(); stateErr == nil {
		finalState = state.MachineState
	}
	message := fmt.Sprintf(
		"run %q made no journal progress for %s (last activity %s; timeout %s)",
		runID,
		now.Sub(lastActivity).Round(time.Second),
		lastActivity.UTC().Format(time.RFC3339),
		timeout,
	)
	if err := jr.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: RunStalledErrorCode, Message: message},
		Runner: map[string]any{
			"lastActivityAt":    lastActivity.UTC().Format(time.RFC3339Nano),
			"stalledRunTimeout": timeout.String(),
		},
	}); err != nil {
		return Result{}, false, fmt.Errorf("runner: journal stalled run %q: %w", runID, err)
	}
	result, err := r.finish(runID, jr, journal.PhaseEscalated, finalState, 0)
	return result, result.Phase == journal.PhaseEscalated, err
}
