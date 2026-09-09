package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func newPersistentDaemonCancelService(layout instance.Layout, runners *daemonRunnerRegistry, log *journal.InstanceLog) (*daemonCancelService, error) {
	if log == nil {
		return nil, errors.New("cancellation requires an audit journal")
	}
	receipts, err := cancelreceipt.Open(filepath.Join(layout.SchedulerDir(), "cancellation-receipts.db"))
	if err != nil {
		return nil, err
	}
	s := newDaemonCancelService(runners)
	s.auditLog, s.receipts = log, receipts
	return s, nil
}

func (s *daemonCancelService) cancelWithReceipt(ctx context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
	// RunID is a path parameter excluded from the body DTO, but part of the
	// idempotent operation's identity. Actor is bound separately by the store.
	payload, err := json.Marshal(struct {
		RunID   string                   `json:"runId"`
		Request httpapi.CancelRunRequest `json:"request"`
	}{input.RunID, input})
	if err != nil {
		return httpapi.CancelRunResult{}, err
	}
	receipt, fresh, err := s.receipts.Begin(ctx, input.IdempotencyKey, input.Actor, payload, time.Now())
	if errors.Is(err, cancelreceipt.ErrConflict) {
		return httpapi.CancelRunResult{}, httpapi.NewInterventionError(http.StatusConflict, "cancel_request_conflict", "request key belongs to another cancellation", nil)
	}
	if err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	if !fresh {
		if !receipt.Complete {
			return httpapi.CancelRunResult{}, httpapi.NewInterventionError(http.StatusConflict, "cancel_outcome_unknown", "cancellation is in progress or its outcome is uncertain; retry the same key or inspect the run before requesting another cancellation", nil)
		}
		var result httpapi.CancelRunResult
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return result, cancelReceiptUnavailable(err)
		}
		return result, nil
	}
	result, err := s.cancelOnce(ctx, input)
	if err != nil {
		return httpapi.CancelRunResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	// A disconnected caller must not prevent recording an already-observed
	// outcome. This bounded write does not extend the cancellation operation.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.receipts.Finish(finishCtx, input.IdempotencyKey, encoded, time.Now()); err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	return result, nil
}

func cancelReceiptUnavailable(err error) error {
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "cancel_receipt_unavailable", "cancellation outcome could not be acknowledged; retry the same key to reconcile", err)
}
