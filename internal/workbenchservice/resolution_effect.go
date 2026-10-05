package workbenchservice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// Resolve records the authorized agent's assessment before one marker attempt.
// Its request key is supplied by the turn-scoped bridge, never a provider key.
func (s *SessionResolver) Resolve(ctx context.Context, binding, key string, request workbench.NeedsHumanResolutionRequest) (workbench.NeedsHumanResolutionCommand, error) {
	request.Evidence = append([]workbench.NeedsHumanEvidenceRef(nil), request.Evidence...)
	var result workbench.NeedsHumanResolutionCommand
	err := s.use(ctx, binding, func(ctx context.Context, bound ReadBinding) error {
		input, err := s.input(bound, binding, key, request)
		if err != nil {
			return err
		}
		replay, settled, err := s.replay(ctx, input)
		if err != nil {
			return err
		}
		if settled {
			result = replay
			return nil
		}
		return s.service.WithLearnedBlock(ctx, resolutionRepository(bound), request.ID, func(ctx context.Context, learned LearnedBlock) error {
			var err error
			result, err = s.resolveLocked(ctx, bound, input, learned)
			return err
		})
	})
	return result, resolutionError(err)
}

// replay is also checked after acquiring claims.lock: another same-key caller
// may have completed while this one was queued behind its provider operation.
func (s *SessionResolver) replay(ctx context.Context, input triggerqueue.NeedsHumanCommandInput) (workbench.NeedsHumanResolutionCommand, bool, error) {
	previous, err := s.service.Queue.FindNeedsHumanCommand(ctx, input.Scope, input.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return workbench.NeedsHumanResolutionCommand{}, false, nil
	}
	if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
		return workbench.NeedsHumanResolutionCommand{}, false, err
	}
	if previous.Input.TargetDigest != input.TargetDigest {
		return workbench.NeedsHumanResolutionCommand{}, false, interactiveaccess.ErrDenied
	}
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, false, err
	}
	previous, _, err = s.service.Queue.AcceptNeedsHumanCommand(ctx, input, s.service.now())
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, false, err
	}
	return resolutionView(previous, true), previous.State != "accepted", nil
}
func (s *SessionResolver) input(bound ReadBinding, binding, key string, request workbench.NeedsHumanResolutionRequest) (triggerqueue.NeedsHumanCommandInput, error) {
	target, err := workbench.BacklogMutationTargetDigest(bound.Scope, bound.Source)
	if err != nil {
		return triggerqueue.NeedsHumanCommandInput{}, err
	}
	operation, err := workbenchprovider.NeedsHumanOperationDigest(bound.Scope, bound.Source, request)
	return triggerqueue.NeedsHumanCommandInput{Scope: s.scope(binding), RequestID: key, TargetDigest: target, OperationDigest: operation, Request: request, Origin: s.origin}, err
}
func (s *SessionResolver) resolveLocked(ctx context.Context, bound ReadBinding, input triggerqueue.NeedsHumanCommandInput, learned LearnedBlock) (workbench.NeedsHumanResolutionCommand, error) {
	replay, settled, err := s.replay(ctx, input)
	if err != nil || settled {
		return replay, err
	}
	adapter, err := s.adapter(ctx, bound)
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	request := input.Request
	observation, err := s.inspect(ctx, adapter, workbench.BacklogItemRequest{ID: request.ID, ExpectedSourceID: request.SourceID}, learned)
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	if observation.Digest != request.ObservationDigest || observation.Item.Revision != request.ExpectedRevision {
		return workbench.NeedsHumanResolutionCommand{}, triggerqueue.ErrConflict
	}
	if len(observation.WaitReasons) > 0 {
		return workbench.NeedsHumanResolutionCommand{}, errResolutionBlocked
	}
	if err = s.verifyEvidence(ctx, input.Scope, request, observation); err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	if err = s.authorize(ctx); err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	input.Observation = &observation
	record, duplicate, err := s.service.Queue.AcceptNeedsHumanCommand(ctx, input, s.service.now())
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	record, claimed, err := s.service.Queue.ClaimNeedsHumanCommand(ctx, input.Scope, record.ID, record.RequestDigest, s.service.now())
	if err != nil {
		return workbench.NeedsHumanResolutionCommand{}, err
	}
	if claimed {
		record, err = s.executeResolution(ctx, adapter, record)
	}
	return resolutionView(record, duplicate), err
}
func (s *SessionResolver) executeResolution(ctx context.Context, adapter *workbenchprovider.AttentionResolver, record triggerqueue.NeedsHumanCommand) (triggerqueue.NeedsHumanCommand, error) {
	receipt := workbench.NeedsHumanResolutionReceipt{OperationDigest: record.Input.OperationDigest, Outcome: "not-applied", RevisionSemantics: record.Input.Observation.Item.RevisionSemantics}
	if err := s.authorize(ctx); err == nil {
		receipt, _ = adapter.Clear(ctx, record.Input.Request, record.Input.OperationDigest)
	}
	// The lease and claims lock remain held until this bounded receipt commit
	// joins, including a lost provider reply or concurrent policy cancellation.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	completed, err := s.service.Queue.CompleteNeedsHumanCommand(cleanup, record.Input.Scope, record.ID, record.RequestDigest, receipt, s.service.now())
	if err != nil {
		return record, err
	}
	return completed, nil
}

// Command returns an exact actor/source receipt without source revision reads.
func (s *SessionResolver) Command(ctx context.Context, binding, id string) (workbench.NeedsHumanResolutionCommand, error) {
	var result workbench.NeedsHumanResolutionCommand
	err := s.use(ctx, binding, func(ctx context.Context, bound ReadBinding) error {
		record, err := s.service.Queue.NeedsHumanCommand(ctx, s.scope(binding), id)
		if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
			return err
		}
		target, targetErr := workbench.BacklogMutationTargetDigest(bound.Scope, bound.Source)
		if targetErr != nil || target != record.Input.TargetDigest {
			return interactiveaccess.ErrDenied
		}
		if err != nil {
			return err
		}
		result = resolutionView(record, false)
		return nil
	})
	return result, resolutionError(err)
}
