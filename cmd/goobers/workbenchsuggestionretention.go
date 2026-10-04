package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func retainSuggestionGenerationPins(ctx context.Context, queue *triggerqueue.Store, pins map[string]bool) error {
	generations, err := queue.RetainedSuggestionGenerations(ctx)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		pins[generation] = true
	}
	return nil
}

func protectSuggestionJournal(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result) error {
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.RunID != candidate.RunID {
		return fmt.Errorf("relationship review retention identity differs")
	}
	if id.Gaggle == "" || !apiv1.ValidRunID(id.RunID) {
		return nil
	}
	retained, err := queue.SuggestionRunRetained(ctx, id.Gaggle, candidate.RunID)
	if err != nil {
		return err
	}
	if retained {
		return fmt.Errorf("relationship review retains producer evidence: %w", retention.ErrCustodyHeld)
	}
	return nil
}
