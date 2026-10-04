// Package restartintent gives human continuation epochs shared durable start custody.
package restartintent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Kind keeps human continuations distinct from ordinary catalog launches.
const Kind = "human-stage-restart/v1"

// Envelope pins a trusted continuation without duplicating its large context.
type Envelope struct {
	Kind             string `json:"kind"`
	Gaggle           string `json:"gaggle"`
	Workflow         string `json:"workflow"`
	SourceRun        string `json:"sourceRun"`
	Epoch            string `json:"epoch"`
	Stage            string `json:"stage"`
	Generation       string `json:"generation"`
	TerminalSequence uint64 `json:"terminalSequence"`
	PlanDigest       string `json:"planDigest"`
}

// Key is the host-selected epoch's idempotency namespace.
func Key(epoch string) string { return "human-restart:" + epoch }

// Service delegates current authorization and execution to the existing human
// runner. BeforePublication is called only after capacity and claims succeed.
type Service struct {
	Queue   *triggerqueue.Store
	Now     func() time.Time
	Launch  func(context.Context, context.Context, runner.StageRestartPlan, func(context.Context) error) error
	Observe func(context.Context, runner.StageRestartPlan) (bool, error)
}

// Accept durably retains the already authorized immutable plan before admission.
func (s *Service) Accept(ctx context.Context, plan runner.StageRestartPlan) (triggerqueue.Record, bool, error) {
	if plan.Source.Child != nil {
		return triggerqueue.Record{}, false, errors.New("restartintent: generated children use child epoch custody")
	}
	raw, err := runner.MarshalStageRestartPlan(plan)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	e := envelope(plan, raw)
	payload, err := json.Marshal(e)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	if e.Generation == "" {
		return triggerqueue.Record{}, false, errors.New("restartintent: missing generation pin")
	}
	return s.Queue.AcceptHumanRestart(ctx, triggerqueue.HumanRestartAcceptance{Key: Key(e.Epoch), Actor: plan.Continuation.Operator, Gaggle: e.Gaggle, SourceRun: e.SourceRun, Stage: e.Stage, Epoch: e.Epoch, TerminalSequence: e.TerminalSequence, Payload: payload, Plan: raw}, s.Now())
}
func envelope(plan runner.StageRestartPlan, raw []byte) Envelope {
	return Envelope{Kind: Kind, Gaggle: plan.Source.Gaggle, Workflow: plan.Source.Workflow, SourceRun: plan.Source.RunID, Epoch: plan.Continuation.RunID, Stage: plan.Continuation.Target, Generation: plan.Source.ConfigGeneration, TerminalSequence: plan.Continuation.ExpectedTerminalSeq, PlanDigest: journal.Digest(raw)}
}

// Load verifies the descriptor, actor and canonical attachment together.
func (s *Service) Load(ctx context.Context, record triggerqueue.Record) (runner.StageRestartPlan, error) {
	raw, err := s.Queue.HumanRestartPlan(ctx, record.ID)
	if err != nil {
		return runner.StageRestartPlan{}, err
	}
	plan, err := runner.ParseStageRestartPlan(raw)
	if err != nil {
		return plan, err
	}
	want, _ := json.Marshal(envelope(plan, raw))
	if !bytes.Equal(want, record.Payload) || record.Actor != plan.Continuation.Operator || record.Key != Key(plan.Continuation.RunID) || plan.Source.Child != nil {
		return runner.StageRestartPlan{}, triggerqueue.ErrConflict
	}
	return plan, nil
}

// Dispatch leaves pre-publication refusals queued. Once the barrier commits, an
// error is uncertain: only exact journal observation can release start custody.
func (s *Service) Dispatch(ctx, execution context.Context, record triggerqueue.Record) error {
	plan, err := s.Load(ctx, record)
	if err != nil {
		return err
	}
	if record.State == triggerqueue.Dispatched {
		return nil
	}
	observed, err := s.Observe(ctx, plan)
	if err != nil {
		return err
	}
	if observed {
		return s.finish(ctx, record, plan)
	}
	if record.State != triggerqueue.Accepted {
		return nil
	}
	if s.Launch == nil {
		return errors.New("restartintent: execution unavailable")
	}
	err = s.Launch(ctx, execution, plan, func(ctx context.Context) error { return s.Queue.BeginDispatch(ctx, record.ID) })
	if err != nil {
		return err
	}
	return s.Reconcile(ctx, record)
}

// Reconcile never treats missing evidence as permission to resend. Local human
// execution recovery owns any published epoch; missing/ambiguous publication
// stays inspectable in dispatching until an ownership-qualified repair exists.
func (s *Service) Reconcile(ctx context.Context, record triggerqueue.Record) error {
	plan, err := s.Load(ctx, record)
	if err != nil {
		return err
	}
	found, err := s.Observe(ctx, plan)
	if err != nil || !found {
		return err
	}
	return s.finish(ctx, record, plan)
}
func (s *Service) finish(ctx context.Context, record triggerqueue.Record, plan runner.StageRestartPlan) error {
	current, err := s.Queue.ByKey(ctx, record.Key)
	if err != nil {
		return err
	}
	if current.State == triggerqueue.Dispatched {
		return nil
	}
	if current.State == triggerqueue.Accepted {
		if err = s.Queue.BeginDispatch(ctx, current.ID); err != nil {
			return err
		}
	}
	return s.Queue.Finish(ctx, current.ID, triggerqueue.Dispatched, plan.Continuation.RunID, "", s.Now())
}
