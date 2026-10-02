package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go.temporal.io/sdk/client"
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

// OperatorMessageDeliverer commits backend receipts to the owning workflow.
// Delivery itself remains in the shared journal backend: replaying an update
// must never cause a second adapter delivery.
type OperatorMessageDeliverer struct {
	client            hitlUpdateClient
	resolveWorkflowID func(context.Context, string) (string, error)
}

// NewOperatorMessageDeliverer uses the daemon's authenticated Temporal client.
func NewOperatorMessageDeliverer(c client.Client) *OperatorMessageDeliverer {
	return &OperatorMessageDeliverer{client: c}
}

// WithWorkflowIDResolver supports scheduled runs whose workflow ID differs.
func (d *OperatorMessageDeliverer) WithWorkflowIDResolver(resolve func(context.Context, string) (string, error)) *OperatorMessageDeliverer {
	next := *d
	next.resolveWorkflowID = resolve
	return &next
}

// Deliver waits for the receipt's durable workflow update completion.
func (d *OperatorMessageDeliverer) Deliver(ctx context.Context, receipt OperatorMessageReceipt) (OperatorMessageReceipt, error) {
	if d == nil || d.client == nil {
		return OperatorMessageReceipt{}, errors.New("operator-message Temporal client unavailable")
	}
	if !apiv1.ValidRunID(receipt.RunID) {
		return OperatorMessageReceipt{}, errors.New("invalid operator-message run")
	}
	if err := receipt.validate(receipt.RunID); err != nil {
		return OperatorMessageReceipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, hitlUpdateTimeout)
	defer cancel()
	result, err := d.submit(ctx, receipt.RunID, receipt)
	if !isHITLNotFound(err) || d.resolveWorkflowID == nil {
		return result, err
	}
	id, resolveErr := d.resolveWorkflowID(ctx, receipt.RunID)
	if resolveErr != nil {
		return OperatorMessageReceipt{}, resolveErr
	}
	if strings.TrimSpace(id) == "" {
		return OperatorMessageReceipt{}, ErrRunNotOpen
	}
	return d.submit(ctx, id, receipt)
}

func (d *OperatorMessageDeliverer) submit(ctx context.Context, workflowID string, receipt OperatorMessageReceipt) (OperatorMessageReceipt, error) {
	handle, err := d.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		UpdateID:   OperatorMessageUpdateName + ":" + receipt.Reference,
		WorkflowID: workflowID, UpdateName: OperatorMessageUpdateName,
		Args: []interface{}{receipt}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return OperatorMessageReceipt{}, err
	}
	var result OperatorMessageReceipt
	if err := handle.Get(ctx, &result); err != nil {
		return OperatorMessageReceipt{}, err
	}
	if result != receipt {
		return OperatorMessageReceipt{}, fmt.Errorf("operator-message workflow returned a different receipt")
	}
	return result, nil
}
