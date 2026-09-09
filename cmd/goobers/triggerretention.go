package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func openTriggerPruneGuard(layout instance.Layout, dryRun bool, now time.Time) (func(retention.Result) error, func(), error) {
	noop := func() {}
	if dryRun {
		return nil, noop, nil
	}
	path := filepath.Join(layout.SchedulerDir(), "accepted-triggers.db")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, noop, nil
	} else if err != nil {
		return nil, noop, err
	}
	queue, err := triggerqueue.Open(path)
	if err != nil {
		return nil, noop, err
	}
	return func(candidate retention.Result) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return acknowledgeTriggerBeforePrune(ctx, queue, candidate, now)
	}, func() { _ = queue.Close() }, nil
}

func acknowledgeTriggerBeforePrune(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result, now time.Time) error {
	record, err := queue.ForRun(ctx, candidate.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.State == triggerqueue.Dispatched && record.RunID == candidate.RunID {
		return nil
	}
	if record.State != triggerqueue.Dispatching || (record.RunID != "" && record.RunID != candidate.RunID) {
		return fmt.Errorf("trigger custody for run %s is inconsistent", candidate.RunID)
	}
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	var payload acceptedTriggerPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return err
	}
	if identity.RunID != candidate.RunID || identity.Workflow != payload.Request.Workflow || (payload.Request.Gaggle != "" && identity.Gaggle != payload.Request.Gaggle) {
		return fmt.Errorf("trigger custody for run %s has mismatched journal identity", candidate.RunID)
	}
	err = queue.Finish(ctx, record.ID, triggerqueue.Dispatched, candidate.RunID, "", now)
	if errors.Is(err, triggerqueue.ErrTransition) {
		// The daemon's ordinary reconciler may have recorded the same receipt.
		current, readErr := queue.ForRun(ctx, candidate.RunID)
		if readErr == nil && current.State == triggerqueue.Dispatched && current.RunID == candidate.RunID {
			return nil
		}
	}
	return err
}
