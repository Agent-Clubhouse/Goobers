package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Startup can retain a new applied generation before daemon queue wiring exists.
// Read already accepted custody here so journal-less requests keep their archive.
func retainedOrdinaryGenerationPins(ctx context.Context, layout instance.Layout) (map[string]bool, error) {
	filename, err := filepath.Abs(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filename); errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	} else if err != nil {
		return nil, err
	}
	queue, err := triggerqueue.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = queue.Close() }()
	return startintent.RetainedGenerations(ctx, queue)
}
