package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/journal"
)

func (u *upSession) configureInteractiveSessions() error {
	if u.durableTriggers == nil || u.setup.RunnerRegistry == nil || u.setup.InteractiveAccess == nil {
		return nil
	}
	runtime := &daemonSessionRuntime{setup: u.setup}
	runtime.replaceGeneration(u.setup.SessionGeneration)
	service := &interactivesession.Service{Queue: u.durableTriggers.queue, Permissions: u.setup.InteractiveAccess, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber()), Pin: runtime.pin, Now: time.Now}
	service.Runtime = &interactivesession.Runtime{Build: runtime.build, Observe: runtime.observe, Reserve: func(ctx context.Context, id journal.RunIdentity, now time.Time) (func(), error) {
		scheduler := u.triggerPlane.sched.Load()
		if scheduler == nil {
			return nil, errors.New("interactive session scheduler unavailable")
		}
		return scheduler.ReserveSession(ctx, id, now)
	}, Restore: func(id journal.RunIdentity) (func(), error) {
		scheduler := u.triggerPlane.sched.Load()
		if scheduler == nil {
			return nil, errors.New("interactive session scheduler unavailable")
		}
		return scheduler.RestoreSession(id)
	}, RegisterDispatch: func() func() { u.wg.Add(1); return u.wg.Done }}
	u.setup.SessionRuntime = runtime
	u.durableTriggers.sessions = service
	u.setup.InteractiveAccess.SetSessionsAvailable(true)
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithInteractiveSessions(service))
	return nil
}

func (s *durableTriggerService) sweepSessions(ctx context.Context) error {
	if s.sessions == nil {
		return nil
	}
	next, err := s.sessions.Sweep(ctx, s.sessionCursor)
	s.sessionCursor = next
	return err
}

// Restore every previously handed-off turn before global admission opens.
// This includes terminal journals with unjoined writers, which the ordinary
// active-run index correctly excludes from ordinary workflow recovery.
func (u *upSession) restoreInteractiveSessions() error {
	if u.durableTriggers == nil || u.durableTriggers.sessions == nil {
		return nil
	}
	cursor := ""
	for {
		ids, err := u.durableTriggers.queue.UnsettledSessionTurns(u.ctx, cursor, 100)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			turn, err := u.durableTriggers.queue.SessionTurn(u.ctx, id)
			if err != nil {
				return err
			}
			if err = u.durableTriggers.reconcileAcceptedRecord(u.ctx, turn.Record); err != nil {
				return err
			}
			cursor = id
		}
	}
}

// The retained generation is shared with the event catalog even when no event
// routes exist; one canonical pin avoids independently recapturing source.
func (d *schedulerDefinitions) sessionGeneration() string { return d.EventCatalog.generation }
func (r *configReloader) publishSchedulerAndSessions(definitions *schedulerDefinitions, now time.Time, digest string) error {
	if err := r.scheduler.Reload(definitions.Entries, definitions.OpenPRRefresher, now, r.appliedDigest, digest); err != nil {
		return err
	}
	if r.setup.SessionRuntime != nil {
		r.setup.SessionRuntime.replaceGeneration(definitions.sessionGeneration())
	}
	return nil
}
