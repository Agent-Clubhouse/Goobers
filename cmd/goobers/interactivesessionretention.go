package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func retainSessionGenerationPins(ctx context.Context, queue *triggerqueue.Store, pins map[string]bool) error {
	generations, err := queue.RetainedSessionGenerations(ctx)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		pins[generation] = true
	}
	return nil
}
func protectSessionJournal(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result) error {
	retained, err := queue.SessionRunRetained(ctx, candidate.RunID)
	if err != nil {
		return err
	}
	if retained {
		return fmt.Errorf("shared session retains execution evidence: %w", retention.ErrCustodyHeld)
	}
	return nil
}
func guardSessionJournalWithoutQueue(candidate retention.Result) error {
	if err := guardParentContributionPrune(candidate); err != nil {
		return err
	}
	rd, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	id, err := rd.Identity()
	if err != nil {
		return err
	}
	if id.Session != nil {
		return fmt.Errorf("shared session receipt store is missing: %w", retention.ErrCustodyHeld)
	}
	return nil
}
