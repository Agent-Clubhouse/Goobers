package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// OperatorMessageUpdateName records a durable backend receipt in run history.
const OperatorMessageUpdateName = "goobers.operator-message.v1"

// OperatorMessageReceipt carries only a reference and a closed-set disposition.
// The authenticated daemon owns the request, content, principal and credentials;
// none of those fields belongs in workflow update arguments or results.
type OperatorMessageReceipt struct {
	RunID     string                     `json:"runId"`
	Reference string                     `json:"reference"`
	State     apiv1.OperatorMessageState `json:"state"`
	Code      string                     `json:"code,omitempty"`
}

// OperatorMessageReference addresses a journal request without serializing an
// operator-controlled idempotency key (which may itself contain a credential).
func OperatorMessageReference(runID, key string) string {
	sum := sha256.Sum256([]byte("goobers.operator-message.v1\x00" + runID + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

func (r OperatorMessageReceipt) validate(runID string) error {
	if r.RunID != runID {
		return errors.New("operator-message run mismatch")
	}
	raw, err := hex.DecodeString(r.Reference)
	if err != nil || len(raw) != sha256.Size {
		return errors.New("invalid operator-message reference")
	}
	switch r.State {
	case apiv1.OperatorMessageState(apiv1.OperatorMessageDelivered), apiv1.OperatorMessageState(apiv1.OperatorMessageFailed),
		apiv1.OperatorMessageState(apiv1.OperatorMessageRejected), apiv1.OperatorMessageState(apiv1.OperatorMessageExpired):
	default:
		return errors.New("operator-message receipt requires a terminal disposition")
	}
	switch r.Code {
	case "", "live_delivery_unsupported", "target_terminal", "target_unavailable", "delivery_failed", "delivery_canceled", "request_expired", "not_authorized":
		return nil
	default:
		return errors.New("invalid operator-message outcome code")
	}
}

func registerOperatorMessages(ctx workflow.Context, runID string) error {
	receipts := make(map[string]OperatorMessageReceipt)
	validate := func(receipt OperatorMessageReceipt) error {
		if err := receipt.validate(runID); err != nil {
			return err
		}
		if previous, ok := receipts[receipt.Reference]; ok && previous != receipt {
			return errors.New("operator-message reference already records another outcome")
		}
		return nil
	}
	return workflow.SetUpdateHandlerWithOptions(ctx, OperatorMessageUpdateName,
		func(_ workflow.Context, receipt OperatorMessageReceipt) (OperatorMessageReceipt, error) {
			if err := validate(receipt); err != nil {
				return OperatorMessageReceipt{}, err
			}
			receipts[receipt.Reference] = receipt
			return receipt, nil
		}, workflow.UpdateHandlerOptions{Validator: validate})
}
