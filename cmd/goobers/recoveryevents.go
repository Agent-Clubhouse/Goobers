package main

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func recoveryRunEvents(layout instance.Layout) func(context.Context, string) ([]journal.Event, error) {
	return func(ctx context.Context, runID string) ([]journal.Event, error) {
		entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
		if err != nil {
			return nil, err
		}
		var events []journal.Event
		for _, entry := range entries {
			if entry.Record.RunID != runID {
				continue
			}
			record, err := recovery.ReadRetainedRecord(entry.RecordPath)
			if err != nil {
				return nil, err
			}
			event, err := recovery.RetainedEvent(record)
			if err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		return events, nil
	}
}
