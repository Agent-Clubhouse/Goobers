package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// acceptedTriggerObserver only acknowledges a matching, durably published run
// identity. Missing, corrupt, ambiguous or mismatched journals never authorize
// replay here. Startup reconciliation of absent runs is a separate decision.
func acceptedTriggerObserver(layout instance.Layout) func(context.Context, triggerqueue.Record) (bool, error) {
	return func(ctx context.Context, record triggerqueue.Record) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		runID := strings.TrimPrefix(record.ID, "trigger-")
		if record.RunID != "" && record.RunID != runID {
			return false, fmt.Errorf("accepted trigger %s has a mismatched dispatch receipt", record.ID)
		}
		dir, err := layout.FindRunDir(runID)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
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
		var payload acceptedTriggerPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return false, err
		}
		if identity.RunID != runID || identity.Workflow != payload.Request.Workflow || (payload.Request.Gaggle != "" && identity.Gaggle != payload.Request.Gaggle) {
			return false, fmt.Errorf("accepted trigger %s has a mismatched run identity", record.ID)
		}
		return true, nil
	}
}

func (s *durableTriggerService) reconcileObserved(ctx context.Context) error {
	if s.observe == nil {
		return nil
	}
	records, err := s.queue.Uncertain(ctx, s.reconcileCursor, 100)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		s.reconcileCursor = ""
		return nil
	}
	var failures error
	for _, record := range records {
		s.reconcileCursor = record.ID
		observed, err := s.observe(ctx, record)
		if err == nil && observed {
			err = s.queue.Finish(ctx, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.dispatch.now())
		}
		failures = errors.Join(failures, err)
	}
	return failures
}
