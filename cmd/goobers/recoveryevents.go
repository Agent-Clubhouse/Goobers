package main

import (
	"context"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func recoveryRunEvents(layout instance.Layout) func(context.Context, string) ([]journal.Event, int, error) {
	return func(ctx context.Context, runID string) ([]journal.Event, int, error) {
		// Observation of the run's OWN records, reported as
		// recovery_observation_failed when it fails. A read bounded by the
		// operator cap refused on an inventory already holding more entries
		// than the cap, so cleanup that had already succeeded was reported as
		// a failure to observe it (#5354). The read is bounded by the
		// structural ceiling and not by the operator cap it reports back:
		// that bound is what the caller checks the observation count against,
		// and it is still the operator's.
		entries, _, limit, err := observeRecoveryInventory(ctx, layout)
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
