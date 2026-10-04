package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/restartintent"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (s *interactiveStageRestart) installQueue(durable *durableTriggerService) {
	if durable == nil {
		return
	}
	s.queued = &restartintent.Service{Queue: durable.queue, Now: time.Now, WaitingReason: restartWaitingReason, Launch: s.launchOrdinaryRestart, Observe: func(ctx context.Context, plan runner.StageRestartPlan) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return intervention.ObserveStageRestart(filepath.Join(s.layout.ForGaggle(plan.Source.Gaggle).RunsDir(), plan.Continuation.RunID), plan)
	}}
	durable.restarts = s.queued
}

func (s *interactiveStageRestart) acceptOrdinaryRestart(ctx context.Context, p httpapi.Principal, plan runner.StageRestartPlan) (intervention.StageRestartAcceptance, error) {
	if s.queued == nil {
		return intervention.StageRestartAcceptance{}, restartRefusal("restart_queue_unavailable", "Human restart queue is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var accepted intervention.StageRestartAcceptance
	err := s.setup.InteractiveAccess.WithRestartAdmission(ctx, p, plan.Source.Gaggle, func(ctx context.Context, load interactiveaccess.RestartSourceLoader) error {
		release, owned := s.setup.RunnerRegistry.acquireChildCustody(plan.Source.RunID)
		if !owned {
			return restartRefusal("restart_source_active", "The source still has an execution or custody owner.")
		}
		defer release()
		if err := stampRestartAuthority(s.layout, p, &plan); err != nil {
			return err
		}
		if _, err := s.preflight(ctx, &plan, load); err != nil {
			return err
		}
		plan.Continuation.VerifySourceBranch = nil
		// Keep the archive pinned until accepted custody can be inventoried.
		_, releaseGeneration, err := pinnedCredentialDefinitions(ctx, s.layout, plan.Source.ConfigGeneration)
		if err != nil {
			return err
		}
		defer releaseGeneration()
		record, duplicate, err := s.queued.Accept(ctx, plan)
		if err != nil {
			return err
		}
		accepted, err = s.restartControlAcceptance(ctx, record, plan.Continuation.RunID, duplicate)
		if err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, triggerqueue.ErrConflict) {
		err = restartRefusal("idempotency_key_reused", "This restart key or source occurrence already belongs to another epoch.")
	}
	if errors.Is(err, triggerqueue.ErrFull) {
		err = httpapi.NewInterventionError(http.StatusServiceUnavailable, "restart_queue_full", "Human restart queue capacity is exhausted; retry the same command.", err)
	}
	return accepted, err
}

// LookupStageRestart checks current permission but does not re-read providers or
// changed source history for an already accepted exact command.
func (s *interactiveStageRestart) LookupStageRestart(ctx context.Context, p httpapi.Principal, source, gaggle string, request runner.StageRestartRequest) (intervention.StageRestartAcceptance, bool, error) {
	if s.queued == nil {
		return intervention.StageRestartAcceptance{}, false, nil
	}
	var accepted intervention.StageRestartAcceptance
	var found bool
	err := s.setup.InteractiveAccess.WithRestartAdmission(ctx, p, gaggle, func(ctx context.Context, _ interactiveaccess.RestartSourceLoader) error {
		record, err := s.queued.Queue.ByKey(ctx, restartintent.Key(request.EpochID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		plan, err := s.queued.Load(ctx, record)
		if err != nil {
			return err
		}
		authority, err := interactiveaccess.ParseRestartAuthority(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName])
		if err != nil {
			return err
		}
		if plan.Source.RunID != source || plan.Source.Gaggle != gaggle || authority.Issuer != p.Issuer || authority.Subject != p.Subject {
			return interactiveaccess.ErrDenied
		}
		if !runner.MatchesStageRestartRequest(plan, request) {
			return restartRefusal("idempotency_key_reused", "This restart key belongs to a different command.")
		}
		found = true
		accepted, err = s.restartControlAcceptance(ctx, record, plan.Continuation.RunID, true)
		if err != nil {
			return err
		}
		return nil
	})
	return accepted, found, err
}

func (s *interactiveStageRestart) launchOrdinaryRestart(ctx, execution context.Context, plan runner.StageRestartPlan, before func(context.Context) error) error {
	authority, err := interactiveaccess.ParseRestartAuthority(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.setup.InteractiveAccess.WithRestartAdmission(ctx, authority.Principal(), plan.Source.Gaggle, func(ctx context.Context, load interactiveaccess.RestartSourceLoader) error {
		release, owned := s.setup.RunnerRegistry.acquireChildCustody(plan.Source.RunID)
		if !owned {
			return restartRefusal("restart_source_active", "The source still has an execution or custody owner.")
		}
		defer release()
		_, err := s.service.LaunchQueuedStageRestart(ctx, execution, plan, func(ctx context.Context, candidate *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) {
			claims, err := s.preflight(ctx, candidate, load)
			if err != nil {
				return nil, err
			}
			// Revalidation can bind the current branch checker, but never select new
			// branch/context bytes after acceptance.
			check := *candidate
			check.Continuation.VerifySourceBranch = nil
			actual, err := runner.MarshalStageRestartPlan(check)
			if err != nil {
				return nil, err
			}
			want, err := runner.MarshalStageRestartPlan(plan)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(actual, want) {
				return nil, restartRefusal("restart_source_changed", "The accepted restart source changed; its epoch remains queued.")
			}
			return claims, nil
		}, before)
		return err
	})
}

func (s *interactiveStageRestart) restartControlAcceptance(ctx context.Context, record triggerqueue.Record, epoch string, duplicate bool) (intervention.StageRestartAcceptance, error) {
	result := intervention.StageRestartAcceptance{RunID: epoch, Duplicate: duplicate, Queued: record.State != triggerqueue.Dispatched, PendingReason: restartPendingReason(record)}
	if record.State != triggerqueue.Rejected {
		return result, nil
	}
	control, err := s.queued.Queue.PinnedStartControl(ctx, record.ID)
	if err != nil {
		return result, err
	}
	if control.Disposition != "cancelled" && control.Disposition != "expired" {
		return result, triggerqueue.ErrTransition
	}
	result.Disposition = control.Disposition
	result.Queued = false
	result.PendingReason = ""
	return result, nil
}
