package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const delegatedTriggerActor = "local-file-delegation"

// The file remains only a response delivery address after queue acceptance.
// Crash recovery can repeat this transfer; the actor/key/request tuple reuses
// the same receipt, pins, deadline and reserved execution identity.
func (s *durableTriggerService) transferDelegated(ctx context.Context, dir, id, active string, req *triggerRequest, existingOnly bool) (bool, error) {
	if s.ordinary == nil {
		return false, errors.New("durable delegated admission unavailable")
	}
	key := "delegated:" + id
	prior, err := s.queue.ByKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) && existingOnly {
		if req.AcceptanceID != "" {
			return true, errors.New("delegated acceptance receipt unavailable")
		}
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	if req.Priority && req.SourceRun == "" {
		return true, startintent.ErrInvalid
	}
	request := startintent.Request{Workflow: req.Workflow, Gaggle: req.Gaggle, Force: req.Force, PullRequest: req.PR}
	if req.Priority {
		request.SourceRun = req.SourceRun
	}
	deadline := s.dispatch.now().Add(acceptedTriggerQueueLifetime())
	if !req.AcceptedAt.IsZero() {
		deadline = req.AcceptedAt.Add(acceptedTriggerQueueLifetime())
	}
	if bound, ok := triggerRequestQueueDeadline(*req, false); ok && bound.Before(deadline) {
		deadline = bound
	}
	// Fence withdrawal before the durable commit, including an unknown reply.
	req.QueueTransfer = true
	if err := requeueTriggerRequest(active, *req); err != nil {
		return true, err
	}
	record, _, err := s.ordinary.AcceptBefore(ctx, key, delegatedTriggerActor, request, deadline)
	if err != nil {
		return true, err
	}
	if prior.ID != "" && prior.ID != record.ID {
		return true, triggerqueue.ErrConflict
	}
	if req.AcceptanceID != "" && req.AcceptanceID != record.ID {
		return true, triggerqueue.ErrConflict
	}
	return true, publishDelegatedReceipt(dir, id, active, req, record)
}

func publishDelegatedReceipt(dir, id, active string, req *triggerRequest, record triggerqueue.Record) error {
	if req.Priority {
		return os.Remove(active)
	}
	if record.State == triggerqueue.Rejected {
		if err := writeTriggerResponse(dir, id, triggerResponse{Error: record.Reason, Retryable: true}); err != nil {
			return err
		}
		return os.Remove(active)
	}
	if record.RunID != "" {
		if err := writeTriggerResponse(dir, id, triggerResponse{RunID: record.RunID}); err != nil {
			return err
		}
		return os.Remove(active)
	}
	req.AcceptanceID = record.ID
	req.AcceptedAt = record.AcceptedAt
	// Persist the custody marker before exposing a new request file, so timeout
	// withdrawal cannot tell the caller an accepted start was cancelled.
	if err := requeueTriggerRequest(filepath.Join(dir, id+requestSuffix), *req); err != nil {
		return err
	}
	if err := writeTriggerAck(dir, id, triggerResponse{State: triggerResponseQueued}); err != nil {
		return err
	}
	return os.Remove(active)
}

func (s *durableTriggerService) retryDelegatedTransfer(dir, id, active string, req triggerRequest, cause error) error {
	// An unavailable acceptance reply is not a rejection. The durable key lets
	// the next sweep discover either the original receipt or proven absence.
	if err := requeueTriggerRequest(filepath.Join(dir, id+requestSuffix), req); err != nil {
		return errors.Join(cause, err)
	}
	_ = os.Remove(active)
	return fmt.Errorf("delegate: durable handoff %s remains retryable: %w", id, cause)
}

type delegatedAdmission func(context.Context, string, string, string, *triggerRequest, bool) (bool, error)

func (s *durableTriggerService) delegatedAdmission() delegatedAdmission {
	return func(ctx context.Context, dir, id, active string, req *triggerRequest, existingOnly bool) (bool, error) {
		handled, err := s.transferDelegated(ctx, dir, id, active, req, existingOnly)
		if errors.Is(err, startintent.ErrInvalid) || errors.Is(err, triggerqueue.ErrConflict) {
			if writeErr := writeTriggerResponse(dir, id, triggerResponse{Error: err.Error()}); writeErr != nil {
				return true, errors.Join(err, writeErr)
			}
			return true, os.Remove(active)
		}
		if err != nil {
			return true, s.retryDelegatedTransfer(dir, id, active, *req, err)
		}
		return handled, nil
	}
}

// queueBound keeps one provider-validation attempt finite; waiting for capacity
// is governed separately by the immutable accepted deadline.
func queueBound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}

// Recover already accepted custody before applying fresh-file expiry and
// suppression. Neither a legacy-only sweep nor malformed input can adopt it.
func recoverDelegatedAdmission(ctx context.Context, admission delegatedAdmission, decodeErr error, dir, id, active string, req *triggerRequest) (bool, error) {
	if admission == nil || decodeErr != nil {
		return false, nil
	}
	return admission(ctx, dir, id, active, req, true)
}
