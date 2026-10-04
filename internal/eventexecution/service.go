package eventexecution

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Prepared is an exact archived entry and its live generation lease.
type Prepared struct {
	Entry   localscheduler.WorkflowEntry
	Release func()
}

// Builder must compile the exact accepted archive and verify both content pins.
type Builder func(context.Context, eventing.StartEnvelope) (Prepared, error)

// Service connects durable custody to ordinary scheduler admission. The host
// supplies strong journal absence, a live scheduler and immutable runtime build.
type Service struct {
	Queue           *triggerqueue.Store
	Build           Builder
	Scheduler       func() *localscheduler.Scheduler
	RunDirectory    func(context.Context, string) (string, error)
	Now             func() time.Time
	AcquireTerminal func(string) (func(), bool)
}

// RouteSweep routes one oldest receipt per selected gaggle and closes due input
// groups. The cursor rotates without retaining a mutable subscription snapshot.
func (s *Service) RouteSweep(ctx context.Context, after string) (string, error) {
	scopes, err := s.Queue.EventRoutingScopes(ctx, after, 100)
	if err != nil {
		return after, err
	}
	if len(scopes) == 0 {
		return "", nil
	}
	var failures error
	for _, gaggle := range scopes {
		if ctx.Err() != nil {
			return after, errors.Join(failures, ctx.Err())
		}
		after = gaggle
		_, err = s.Queue.RouteNextEvent(ctx, gaggle, s.Now())
		failures = errors.Join(failures, err)
		_, err = s.Queue.CloseEventGroups(ctx, gaggle, s.Now(), 1)
		failures = errors.Join(failures, err)
	}
	return after, failures
}

// Dispatch keeps uncertain handoffs claimed until a matching journal is seen.
// Known admission refusal requeues without consuming another logical identity.
func (s *Service) Dispatch(admission, execution context.Context, record triggerqueue.Record) error {
	start, err := eventing.ParseStart(record.Payload)
	if err != nil {
		return err
	}
	verified, inputs, err := Inputs(admission, s.Queue, start.Gaggle, start.GroupID)
	if err != nil {
		return err
	}
	if verified.ID != record.ID || verified.State != triggerqueue.Accepted {
		return triggerqueue.ErrTransition
	}
	if s.Build == nil || s.Scheduler == nil {
		return nil
	}
	scheduler := s.Scheduler()
	if scheduler == nil {
		return nil
	}
	if err := checkInputRedaction(inputs); err != nil {
		return err
	}
	prepared, err := s.Build(admission, start)
	if err != nil {
		return err
	}
	starter := &Starter{Next: prepared.Entry.Starter, Inputs: inputs, Release: prepared.Release}
	handedOff := false
	defer func() {
		if !handedOff {
			starter.Close()
		}
	}()
	prepared.Entry.Starter = starter
	if observed, _, err := s.Observe(admission, record); err != nil || observed {
		return errors.Join(err, errors.New("eventexecution: published run must be reconciled"))
	}
	if err = s.Queue.BeginDispatch(admission, record.ID); err != nil {
		return err
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	admitted, err := scheduler.TriggerPrepared(execution, prepared.Entry, runID, journal.Trigger{Kind: journal.TriggerSignal, Ref: "event:" + start.GroupID}, s.Now())
	if err != nil {
		return errors.Join(err, s.Queue.Requeue(admission, record.ID, "event execution admission unavailable"))
	}
	handedOff = true
	if admitted != runID {
		return errors.New("eventexecution: scheduler changed reserved run identity")
	}
	return s.Queue.RecordDispatch(admission, record.ID, runID)
}

// Reconcile claims only exact published provenance; only startup-owned absence
// permits a previously uncertain start to become eligible again.
func (s *Service) Reconcile(ctx context.Context, record triggerqueue.Record, allowAbsentReplay bool) error {
	observed, _, err := s.Observe(ctx, record)
	if err != nil {
		return err
	}
	if observed {
		return s.Queue.Finish(ctx, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.Now())
	}
	if allowAbsentReplay {
		return s.Queue.RetryUnstarted(ctx, record.ID)
	}
	return nil
}

// SettleSweep retains paused/escalated runs and accepts only terminal evidence.
func (s *Service) SettleSweep(ctx context.Context, after string) (string, error) {
	groups, err := s.Queue.UnsettledEventGroups(ctx, after, 100)
	if err != nil {
		return after, err
	}
	if len(groups) == 0 {
		return "", nil
	}
	var failures error
	for _, group := range groups {
		if ctx.Err() != nil {
			return after, errors.Join(failures, ctx.Err())
		}
		after = group.ID
		failures = errors.Join(failures, s.settle(ctx, group))
	}
	return after, failures
}

func (s *Service) settle(ctx context.Context, group triggerqueue.EventGroup) error {
	record, _, err := s.Queue.VerifiedEventStart(ctx, group.Gaggle, group.ID)
	if err != nil {
		return err
	}
	if record.State == triggerqueue.Accepted {
		return nil
	}
	observed, outcome, err := s.Observe(ctx, record)
	if err != nil {
		return err
	}
	if record.State == triggerqueue.Rejected {
		if observed {
			return errors.New("eventexecution: rejected start has a run journal")
		}
		return s.Queue.SettleEventGroup(ctx, group.Gaggle, group.ID, "", "rejected", s.Now())
	}
	if !observed {
		return nil
	}
	if record.State == triggerqueue.Dispatching {
		if err = s.Queue.Finish(ctx, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.Now()); err != nil {
			return err
		}
	}
	if outcome == "" {
		return nil
	}
	return s.Queue.SettleEventGroup(ctx, group.Gaggle, group.ID, strings.TrimPrefix(record.ID, "trigger-"), outcome, s.Now())
}
