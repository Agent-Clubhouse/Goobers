package livejournal

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TerminalInterventionRequest is a trusted workflow-host transition, never an
// ordinary stage emission. Its durable marker binds all retries to one intent.
type TerminalInterventionRequest struct {
	RunID      string                       `json:"runId"`
	Gaggle     string                       `json:"gaggle"`
	Transition journal.TerminalIntervention `json:"transition"`
}

// CommitTerminalIntervention retains a terminal writer only for the backend's
// fenced validation and commit. The ordinary Emit path still refuses stray
// post-terminal events, including forged resume-shaped append operations.
func (w *Writer) CommitTerminalIntervention(ctx context.Context, req TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	if !apiv1.ValidRunID(req.RunID) {
		return journal.TerminalInterventionReceipt{}, errors.New("livejournal: invalid intervention run id")
	}
	if err := journal.ValidateTerminalIntervention(req.Transition); err != nil {
		return journal.TerminalInterventionReceipt{}, err
	}
	batch := EmitRequest{RunID: req.RunID, Gaggle: req.Gaggle, Ops: []Op{{Time: req.Transition.Events[0].Time}}}
	run, err := w.acquireForTransition(ctx, batch, true)
	if err != nil {
		return journal.TerminalInterventionReceipt{}, err
	}
	run.mu.Lock()
	for !w.hasRun(run) {
		run.mu.Unlock()
		// A concurrent terminal emit may have retired the handle acquired above.
		// Drop its cache entry and acquire a fresh fenced incarnation.
		w.forgetInterventionRun(req.RunID, run)
		run, err = w.acquireForTransition(ctx, batch, true)
		if err != nil {
			return journal.TerminalInterventionReceipt{}, err
		}
		run.mu.Lock()
	}
	defer run.mu.Unlock()
	defer func() {
		if w.hasRun(run) || run.adopted {
			w.finishRun(req.RunID, run, &EmitResponse{})
		}
	}()
	if err := w.validateReservationIdentity(ctx, run, batch); err != nil {
		return journal.TerminalInterventionReceipt{}, err
	}
	run.lastEmit = w.now()
	if run.clock != nil {
		run.clock.set(req.Transition.Events[0].Time)
	}
	before := w.seq(run)
	receipt, err := w.commitIntervention(ctx, run, req.Transition)
	if err != nil {
		// An ordered file prefix may have committed. Forget cached assumptions;
		// only the same durable intent may complete it after recovery.
		w.discardInterventionRun(req.RunID, run)
		return journal.TerminalInterventionReceipt{}, err
	}
	reader, err := w.runReader(ctx, run)
	if err == nil {
		var events []journal.Event
		events, err = reader.Events()
		if err == nil {
			deriveDedupState(run, events)
			for _, event := range events {
				if event.Seq > before && !receipt.Duplicate {
					w.notifyEvent(req.RunID, event)
				}
			}
		}
	}
	if err != nil {
		w.discardInterventionRun(req.RunID, run)
		return journal.TerminalInterventionReceipt{}, fmt.Errorf("livejournal: refresh intervention receipt: %w", err)
	}
	return receipt, nil
}

func (w *Writer) commitIntervention(ctx context.Context, run *liveRun, req journal.TerminalIntervention) (journal.TerminalInterventionReceipt, error) {
	if run.adopted && run.jr != nil {
		return run.jr.CommitTerminalIntervention(req)
	}
	backend, ok := w.backend.(journal.TerminalInterventionBackend)
	if !ok {
		return journal.TerminalInterventionReceipt{}, errors.New("livejournal: terminal intervention backend unavailable")
	}
	return backend.CommitTerminalIntervention(ctx, run.handle, run.owner, req)
}

func (w *Writer) forgetInterventionRun(runID string, run *liveRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.open[runID] == run {
		delete(w.open, runID)
	}
}
func (w *Writer) discardInterventionRun(runID string, run *liveRun) {
	if run.adopted {
		return
	}
	_ = w.retire(run)
	w.clearRun(run)
	w.forgetInterventionRun(runID, run)
}
