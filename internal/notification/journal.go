package notification

import (
	"context"
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// JournalRecorder stores requests and receipts in a run's append-only event log.
// The run scrubber is therefore applied before either contract reaches disk.
type JournalRecorder struct {
	run      *journal.Run
	runID    string
	workflow string
	mu       sync.Mutex
}

func NewJournalRecorder(run *journal.Run) (*JournalRecorder, error) {
	if run == nil {
		return nil, fmt.Errorf("notification: run journal is required")
	}
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		return nil, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	return &JournalRecorder{run: run, runID: identity.RunID, workflow: identity.Workflow}, nil
}

func (r *JournalRecorder) RecordRequest(ctx context.Context, request apiv1.NotificationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Source.RunID != r.runID || request.Source.Workflow != r.workflow {
		return fmt.Errorf("notification: request source does not match run journal")
	}
	return r.run.Append(journal.Event{
		Type:                journal.EventNotificationRequested,
		NotificationRequest: &request,
	})
}

func (r *JournalRecorder) RecordReceipt(ctx context.Context, receipt apiv1.NotificationReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recordReceipt(ctx, receipt)
}

func (r *JournalRecorder) recordReceipt(ctx context.Context, receipt apiv1.NotificationReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if receipt.Source.RunID != r.runID || receipt.Source.Workflow != r.workflow {
		return fmt.Errorf("notification: receipt source does not match run journal")
	}
	return r.run.Append(journal.Event{
		Type:                journal.EventNotificationReceipt,
		NotificationReceipt: &receipt,
	})
}

func (r *JournalRecorder) ClaimDelivery(ctx context.Context, pending apiv1.NotificationReceipt) (apiv1.NotificationReceipt, RecordedDeliveryState, error) {
	if err := ctx.Err(); err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reader, err := journal.OpenRead(r.run.Dir())
	if err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	events, err := reader.Events()
	if err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		receipt := events[i].NotificationReceipt
		if receipt == nil || receipt.IdempotencyDigest != pending.IdempotencyDigest ||
			receipt.Sink.Kind != pending.Sink.Kind || receipt.Attempt == 0 {
			continue
		}
		switch {
		case receipt.Status == apiv1.NotificationDelivered:
			return *receipt, DeliveryComplete, nil
		case receipt.Status == apiv1.NotificationPending || receipt.Unresolved:
			return *receipt, DeliveryUnresolved, nil
		default:
			if err := r.recordReceipt(ctx, pending); err != nil {
				return apiv1.NotificationReceipt{}, DeliveryClaimed, err
			}
			return apiv1.NotificationReceipt{}, DeliveryClaimed, nil
		}
	}
	if err := r.recordReceipt(ctx, pending); err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	return apiv1.NotificationReceipt{}, DeliveryClaimed, nil
}
