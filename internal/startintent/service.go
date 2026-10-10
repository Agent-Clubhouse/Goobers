package startintent

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Prepared holds a compiled archived entry and a lease through execution.
type Prepared struct {
	Entry   localscheduler.WorkflowEntry
	Release func()
}

// Service uses the one shared ledger, scheduler and retained generation store.
// Capture must return a live archive lease before its applied snapshot unlocks.
type Service struct {
	Queue        *triggerqueue.Store
	Capture      func(context.Context, Request) (Target, func(), error)
	Build        func(context.Context, Target) (Prepared, error)
	Scheduler    func() *localscheduler.Scheduler
	RunDirectory func(context.Context, string) (string, error)
	Now          func() time.Time
}

// Accept captures one ordinary selection under an archive lease. Retries retain
// the original pins even if the applied configuration has changed.
func (s *Service) Accept(ctx context.Context, key, actor string, request Request) (triggerqueue.Record, bool, error) {
	if err := request.Validate(); err != nil {
		return triggerqueue.Record{}, false, err
	}
	if prior, err := s.Queue.ByKey(ctx, key); err == nil {
		return duplicate(prior, actor, request)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return triggerqueue.Record{}, false, err
	}
	if s.Capture == nil {
		return triggerqueue.Record{}, false, errors.New("startintent: capture unavailable")
	}
	target, release, err := s.Capture(ctx, request)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	if release == nil {
		return triggerqueue.Record{}, false, errors.New("startintent: archive lease unavailable")
	}
	defer release()
	raw, err := (Envelope{Kind: Kind, Request: request, Target: target}).Marshal()
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	record, repeated, err := s.Queue.Accept(ctx, key, actor, raw, s.Now())
	if errors.Is(err, triggerqueue.ErrConflict) {
		if prior, readErr := s.Queue.ByKey(ctx, key); readErr == nil {
			return duplicate(prior, actor, request)
		}
	}
	return record, repeated, err
}

func duplicate(record triggerqueue.Record, actor string, request Request) (triggerqueue.Record, bool, error) {
	prior, err := Parse(record.Payload)
	if err != nil || record.Actor != actor || prior.Request != request || prior.Source != nil {
		return triggerqueue.Record{}, false, triggerqueue.ErrConflict
	}
	return record, true, nil
}

// Dispatch keeps custody after the scheduler may have launched. Only an
// observed matching journal, or startup-proven absence, resolves uncertainty.
func (s *Service) Dispatch(admission, execution context.Context, record triggerqueue.Record) error {
	e, err := Parse(record.Payload)
	if err != nil {
		return err
	}
	if s.Build == nil || s.Scheduler == nil {
		return nil
	}
	scheduler := s.Scheduler()
	if scheduler == nil {
		return nil
	}
	prepared, err := s.Build(admission, e.Target)
	if err != nil {
		return err
	}
	if prepared.Release == nil {
		return errors.New("startintent: execution archive lease unavailable")
	}
	starter := &leasedStarter{next: prepared.Entry.Starter, release: prepared.Release}
	handedOff := false
	defer func() {
		if !handedOff {
			starter.close()
		}
	}()
	prepared.Entry.Starter = starter
	if observed, err := s.Observe(admission, record); err != nil {
		return err
	} else if observed {
		if err := s.Queue.BeginDispatch(admission, record.ID); err != nil {
			return err
		}
		return s.Queue.Finish(admission, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.Now())
	}
	if err = s.Queue.BeginDispatch(admission, record.ID); err != nil {
		return err
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	admitted, err := scheduler.TriggerPreparedOrdinary(admission, execution, prepared.Entry, runID, localscheduler.PreparedTriggerOptions{Force: e.Request.Force, SourceRun: e.Request.SourceRun, Source: e.Source}, s.Now())
	if err != nil {
		return s.refuseDispatch(admission, record.ID, err)
	}
	handedOff = true
	if admitted != runID {
		return errors.New("startintent: scheduler changed reserved run identity")
	}
	return s.Queue.RecordDispatch(admission, record.ID, runID)
}

func (s *Service) refuseDispatch(ctx context.Context, id string, err error) error {
	// The scheduler returned before launching. Finish this bounded custody
	// update even when its provider-validation context just expired.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return s.Queue.Requeue(cleanup, id, "ordinary start validation interrupted")
	}
	ctx = cleanup
	var rejected *localscheduler.TriggerRejectedError
	if errors.As(err, &rejected) {
		if rejected.Transient() {
			return s.Queue.Requeue(ctx, id, rejected.Reason)
		}
		return s.Queue.Finish(ctx, id, triggerqueue.Rejected, "", rejected.Reason, s.Now())
	}
	// Invalid selectors/provider validation are definitive before dispatch.
	return s.Queue.Finish(ctx, id, triggerqueue.Rejected, "", err.Error(), s.Now())
}
