package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func protectChildJournal(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result) error {
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.RunID != candidate.RunID {
		return fmt.Errorf("child retention run identity mismatch")
	}
	owned, err := queue.ChildJournalOwned(ctx, id.Gaggle, id.RunID)
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("run %s remains owned by retained child-family custody", candidate.RunID)
	}
	return nil
}
