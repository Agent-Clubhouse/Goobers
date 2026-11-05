package runner

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
)

func (r *Runner) refuseSelfExecution(ctx context.Context, jr executionJournal, in StartInput, stage string) error {
	err := &runsemantics.SelfExecutionRefusal{Stage: stage}
	if r.cfg.SelfExecutionObserved != nil {
		r.cfg.SelfExecutionObserved(true)
	}
	if appendErr := jr.Append(journal.Event{Type: journal.EventError, Stage: stage, Error: journal.ErrorDetailFor(runsemantics.SelfExecutionDeniedCode, err)}); appendErr != nil {
		return appendErr
	}
	outcome := BlockedOutcome{RunID: in.RunID, RepoRef: in.RepoRef, Stage: stage, Reason: err.Error()}
	if in.Item != nil {
		outcome.ItemID = in.Item.ID
	}
	if run, ok := jr.(*journal.Run); ok {
		if notifyErr := r.notifyStageEscalation(ctx, run, in.RunID, in.Item, in.RepoRef, stage, err.Error()); notifyErr != nil {
			return notifyErr
		}
	}
	if r.cfg.Blocked != nil {
		if blockErr := r.cfg.Blocked(ctx, outcome); blockErr != nil {
			if appendErr := jr.Append(journal.Event{Type: journal.EventError, Stage: stage, Error: journal.ErrorDetailFor("blocked_handling_failed", blockErr)}); appendErr != nil {
				return appendErr
			}
		}
	}
	return err
}

func (r *Runner) observeSelfExecution(refused bool) {
	if r.cfg.SelfExecutionObserved != nil {
		r.cfg.SelfExecutionObserved(refused)
	}
}

func (r *Runner) refuseSelfTask(tf taskFrame) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	r.observeSelfExecution(true)
	result := runsemantics.SelfExecutionBlockedResult(tf.t.Name)
	if err := tf.jr.Append(journal.Event{Type: journal.EventError, Stage: tf.t.Name, Error: journal.ErrorDetailFor(runsemantics.SelfExecutionDeniedCode, &runsemantics.SelfExecutionRefusal{Stage: tf.t.Name})}); err != nil {
		return apiv1.ResultEnvelope{}, nil, err
	}
	return result, nil, nil
}

func (r *Runner) admitSelfGate(ctx context.Context, jr executionJournal, in StartInput, g apiv1.Gate) error {
	if g.Evaluator != apiv1.EvaluatorAgentic {
		return nil
	}
	if r.cfg.SelfExecutionDenied {
		return r.refuseSelfExecution(ctx, jr, in, g.Name)
	}
	r.observeSelfExecution(false)
	return nil
}
