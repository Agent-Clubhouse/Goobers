package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func (s *schedulerSetup) installQueuedEvents(layout instance.Layout, triggers *durableTriggerService) error {
	if s.EventRuntime == nil || s.RunnerRegistry == nil || triggers == nil || triggers.queue == nil || triggers.dispatch == nil {
		return errors.New("event execution coordination unavailable")
	}
	service := &eventexecution.Service{Queue: triggers.queue, Build: func(ctx context.Context, start eventing.StartEnvelope) (eventexecution.Prepared, error) {
		runtime, err := s.EventRuntime(ctx, start)
		return runtime.Prepared, err
	},
		Scheduler: func() *localscheduler.Scheduler {
			if triggers.dispatch.schedulerReady != nil && !triggers.dispatch.schedulerReady() {
				return nil
			}
			return triggers.dispatch.sched.Load()
		},
		RunDirectory: func(ctx context.Context, runID string) (string, error) {
			return acceptedTriggerJournalDir(ctx, layout, runID)
		}, Now: triggers.dispatch.now, AcquireTerminal: s.RunnerRegistry.acquireChildCustody}
	triggers.events = service
	s.RunnerRegistry.mu.Lock()
	s.RunnerRegistry.resolveEventGeneration = func(ctx context.Context, id journal.RunIdentity) (executionGenerationRuntime, error) {
		return s.resolveEventGeneration(ctx, layout, triggers, id)
	}
	s.RunnerRegistry.mu.Unlock()
	return nil
}

func (s *schedulerSetup) resolveEventGeneration(ctx context.Context, layout instance.Layout, triggers *durableTriggerService, id journal.RunIdentity) (executionGenerationRuntime, error) {
	if id.Event == nil {
		return executionGenerationRuntime{}, errors.New("event recovery lacks provenance")
	}
	record, start, err := triggers.queue.VerifiedEventStart(ctx, id.Gaggle, id.Event.GroupID)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	dir, err := acceptedTriggerJournalDir(ctx, layout, id.RunID)
	if err != nil || dir == "" {
		return executionGenerationRuntime{}, errors.Join(err, errors.New("event recovery journal missing"))
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	if err = eventexecution.VerifyIdentity(rd, id, record, start); err != nil {
		return executionGenerationRuntime{}, err
	}
	if err := triggers.events.VerifyMembership(ctx, rd, id, start); err != nil {
		return executionGenerationRuntime{}, err
	}
	runtime, err := s.EventRuntime(ctx, start)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	defer runtime.Release()
	sched := triggers.dispatch.sched.Load()
	if sched == nil {
		return executionGenerationRuntime{}, errors.New("event recovery scheduler unavailable")
	}
	if _, err = sched.PreparedEntry(runtime.Entry); err != nil {
		return executionGenerationRuntime{}, err
	}
	return runtime.executionGenerationRuntime, nil
}

func deferEventGenerationRecovery(ctx context.Context, log *journal.InstanceLog, id journal.RunIdentity) bool {
	if id.Event == nil || ctx.Err() != nil {
		return false
	}
	if log != nil {
		log.AppendBestEffort(journal.Event{Type: journal.EventError, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: id.RunID, Error: &journal.ErrorDetail{Code: "event_recovery_deferred", Message: "event recovery refused retained input, execution pins, or current eligibility; repair custody and retry recovery"}})
	}
	return true
}

// Routing is enabled only after the shared dependency inventories protect
// journals and generation archives. One bounded sweep cannot block ingress.
func (s *durableTriggerService) sweepEvents(ctx context.Context) error {
	if s.events == nil {
		return nil
	}
	// Separate bounded budgets prevent sustained routing from starving terminal
	// custody and retention. Every phase remains bounded within this daemon tick.
	routeErr := boundedEventSweep(ctx, func(scope context.Context) error {
		var err error
		s.eventScopeCursor, err = s.events.RouteSweep(scope, s.eventScopeCursor)
		return err
	})
	settleErr := boundedEventSweep(ctx, func(scope context.Context) error {
		var err error
		s.eventGroupCursor, err = s.events.SettleSweep(scope, s.eventGroupCursor)
		return err
	})
	publicationErr := boundedEventSweep(ctx, func(scope context.Context) error {
		var err error
		s.eventPublicationCursor, err = s.events.SettlePublicationSweep(scope, s.eventPublicationCursor)
		return err
	})
	pruneErr := boundedEventSweep(ctx, func(scope context.Context) error {
		_, err := s.queue.PruneEventPublications(scope, s.dispatch.now(), 100)
		return err
	})
	return errors.Join(routeErr, settleErr, publicationErr, pruneErr)
}

func boundedEventSweep(ctx context.Context, work func(context.Context) error) error {
	sweep, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return work(sweep)
}
