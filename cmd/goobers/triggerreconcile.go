package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func acceptedTriggerObserver(layout instance.Layout) func(context.Context, triggerqueue.Record) (bool, error) {
	return func(ctx context.Context, record triggerqueue.Record) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		runID := strings.TrimPrefix(record.ID, "trigger-")
		if record.RunID != "" && record.RunID != runID {
			return false, fmt.Errorf("accepted trigger %s has a mismatched dispatch receipt", record.ID)
		}
		dir, err := acceptedTriggerJournalDir(ctx, layout, runID)
		if err != nil || dir == "" {
			return false, err
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return false, err
		}
		identity, err := reader.Identity()
		if err != nil {
			return false, err
		}
		var payload struct {
			Request httpapi.TriggerRequest `json:"request"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return false, err
		}
		if identity.RunID != runID || identity.Workflow != payload.Request.Workflow ||
			(payload.Request.Gaggle != "" && identity.Gaggle != payload.Request.Gaggle) {
			return false, fmt.Errorf("accepted trigger %s has a mismatched run identity", record.ID)
		}
		return true, nil
	}
}

func (s *durableTriggerService) CanReconcileAccepted() bool {
	return s.observeChild != nil || s.ordinary != nil
}

func (s *durableTriggerService) ReconcileAccepted(ctx context.Context, record triggerqueue.Record, kind string) (*bool, error) {
	switch kind {
	case childworkflow.ChildStartKind:
		return nil, s.reconcileChildReceipt(ctx, record)
	case startintent.Kind:
		if s.ordinary == nil {
			return nil, errors.New("ordinary start custody unavailable")
		}
		observed, err := s.ordinary.Observe(ctx, record)
		return &observed, err
	default:
		return nil, errors.New("unsupported accepted trigger envelope kind")
	}
}
