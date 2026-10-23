package planes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
)

// Cancellation receipt error codes are shared by the producer and retrying client.
const (
	CancelRequestConflictCode    = "cancel_request_conflict"
	CancelOutcomeUnknownCode     = "cancel_outcome_unknown"
	CancelReceiptUnavailableCode = "cancel_receipt_unavailable"
)

// CancelReceiptService makes run cancellation idempotent through durable receipts.
type CancelReceiptService struct {
	receipts *cancelreceipt.Store
	cancel   func(context.Context, httpapi.CancelRunRequest) (httpapi.CancelRunResult, error)
}

// NewCancelReceiptService binds a receipt store to the underlying cancellation operation.
func NewCancelReceiptService(receipts *cancelreceipt.Store, cancel func(context.Context, httpapi.CancelRunRequest) (httpapi.CancelRunResult, error)) *CancelReceiptService {
	return &CancelReceiptService{receipts: receipts, cancel: cancel}
}

// Cancel performs or replays one idempotent cancellation.
func (s *CancelReceiptService) Cancel(ctx context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
	payload, err := json.Marshal(struct {
		RunID   string                   `json:"runId"`
		Request httpapi.CancelRunRequest `json:"request"`
	}{input.RunID, input})
	if err != nil {
		return httpapi.CancelRunResult{}, err
	}
	receipt, fresh, err := s.receipts.Begin(ctx, input.IdempotencyKey, input.Actor, payload, time.Now())
	if errors.Is(err, cancelreceipt.ErrConflict) {
		return httpapi.CancelRunResult{}, httpapi.NewInterventionError(http.StatusConflict, CancelRequestConflictCode, "request key belongs to another cancellation", nil)
	}
	if err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	if !fresh {
		if !receipt.Complete {
			return httpapi.CancelRunResult{}, httpapi.NewInterventionError(http.StatusConflict, CancelOutcomeUnknownCode, "cancellation is in progress or its outcome is uncertain; retry the same key or inspect the run before requesting another cancellation", nil)
		}
		var result httpapi.CancelRunResult
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return result, cancelReceiptUnavailable(err)
		}
		return result, nil
	}
	result, err := s.cancel(ctx, input)
	if err != nil {
		return httpapi.CancelRunResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.receipts.Finish(finishCtx, input.IdempotencyKey, encoded, time.Now()); err != nil {
		return httpapi.CancelRunResult{}, cancelReceiptUnavailable(err)
	}
	return result, nil
}

func cancelReceiptUnavailable(err error) error {
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, CancelReceiptUnavailableCode, "cancellation outcome could not be acknowledged; retry the same key to reconcile", err)
}
