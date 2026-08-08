package notification

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// JournalRecorder stores requests and receipts in a run's append-only event log.
// The run scrubber is therefore applied before either contract reaches disk.
type JournalRecorder struct {
	run      *journal.Run
	runID    string
	workflow string
}

// NewJournalRecorder creates a recorder backed by the supplied run journal.
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

// RecordRequest appends a notification request to the run journal.
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

// RecordReceipt appends a notification delivery receipt to the run journal.
func (r *JournalRecorder) RecordReceipt(ctx context.Context, receipt apiv1.NotificationReceipt) error {
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

// ClaimDelivery atomically records a pending delivery or returns its prior durable state.
func (r *JournalRecorder) ClaimDelivery(ctx context.Context, pending apiv1.NotificationReceipt) (apiv1.NotificationReceipt, RecordedDeliveryState, error) {
	if err := ctx.Err(); err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	if pending.Source.RunID != r.runID || pending.Source.Workflow != r.workflow {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, fmt.Errorf("notification: receipt source does not match run journal")
	}
	existing, err := r.run.ClaimNotificationDelivery(pending)
	if err != nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, err
	}
	if existing == nil {
		return apiv1.NotificationReceipt{}, DeliveryClaimed, nil
	}
	if existing.Status == apiv1.NotificationDelivered {
		return *existing, DeliveryComplete, nil
	}
	return *existing, DeliveryUnresolved, nil
}
