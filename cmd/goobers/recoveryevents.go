package main

import (
	"context"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func recoveryRunEvents(layout instance.Layout) func(context.Context, string) ([]journal.Event, int, error) {
	return func(ctx context.Context, runID string) ([]journal.Event, int, error) {
		entries, limit, err := readConfiguredRecoveryInventory(ctx, layout)
		if err != nil {
			return nil, limit, err
		}
		var events []journal.Event
		for _, entry := range entries {
			if entry.Record.RunID != runID {
				continue
			}
			record, err := recovery.ReadRetainedRecord(entry.RecordPath)
			if err != nil {
				return nil, limit, err
			}
			event, err := recovery.RetainedEvent(record)
			if err != nil {
				return nil, limit, err
			}
			events = append(events, event)
		}
		return events, limit, nil
	}
}
