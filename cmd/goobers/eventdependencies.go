package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Every generation-pruning entry point uses this inventory, including startup
// composition before the daemon has opened its long-lived queue connection.
func retainEventGenerationPins(ctx context.Context, layout instance.Layout, pins map[string]bool) error {
	path, err := filepath.Abs(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	queue, err := triggerqueue.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()
	if err := retainSessionGenerationPins(ctx, queue, pins); err != nil {
		return err
	}
	dependencies, err := eventexecution.RetainedDependencies(ctx, queue)
	if err != nil {
		return fmt.Errorf("retained event generation custody: %w", err)
	}
	ordinary, err := startintent.RetainedGenerations(ctx, queue)
	if err != nil {
		return fmt.Errorf("retained ordinary generation custody: %w", err)
	}
	for generation := range ordinary {
		pins[generation] = true
	}
	for generation := range dependencies.Generations {
		pins[generation] = true
	}
	return nil
}

func protectEventJournal(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result) error {
	dependencies, err := eventexecution.RetainedDependencies(ctx, queue)
	if err != nil {
		return fmt.Errorf("retained event journal custody: %w", err)
	}
	if len(dependencies.Runs) == 0 {
		return nil
	}
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.RunID != candidate.RunID {
		return errors.New("event retention candidate identity differs")
	}
	if dependencies.Runs[eventexecution.RunRef{Gaggle: id.Gaggle, RunID: id.RunID}] {
		return fmt.Errorf("event inputs or root budget retain run %s: %w", id.RunID, retention.ErrCustodyHeld)
	}
	return nil
}
