// Package eventexecution owns retained event execution inputs and lifecycle.
package eventexecution

import (
	"context"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// RunRef preserves the gaggle boundary when matching journal dependencies.
type RunRef struct{ Gaggle, RunID string }

// Dependencies is the complete bounded-store retention inventory. Callers must
// discard partial results after any read error; missing custody is never proof
// that an archive or journal can be removed.
type Dependencies struct {
	Generations map[string]bool
	Runs        map[RunRef]bool
}

// RetainedDependencies includes accepted-but-unrouted events and workflow-root
// producers whose budget custody outlives ordinary receipt history.
func RetainedDependencies(ctx context.Context, queue *triggerqueue.Store) (Dependencies, error) {
	result := Dependencies{Generations: map[string]bool{}, Runs: map[RunRef]bool{}}
	for after := ""; ; {
		page, err := queue.EventDependencyPage(ctx, after, 100)
		if err != nil {
			return Dependencies{}, err
		}
		for _, pin := range page {
			for _, generation := range pin.ConfigGenerations {
				result.Generations[generation] = true
			}
			if pin.SourceRunID != "" {
				result.Runs[RunRef{Gaggle: pin.Gaggle, RunID: pin.SourceRunID}] = true
			}
			for _, runID := range pin.ConsumerRunIDs {
				result.Runs[RunRef{Gaggle: pin.Gaggle, RunID: runID}] = true
			}
			after = pin.ReceiptID
		}
		if len(page) < 100 {
			break
		}
	}
	for after := ""; ; {
		page, err := queue.EventPublicationPage(ctx, after, 100)
		if err != nil {
			return Dependencies{}, err
		}
		for _, pin := range page {
			result.Generations[pin.ConfigGeneration] = true
			result.Runs[RunRef{Gaggle: pin.Acceptance.Producer.Gaggle, RunID: pin.Acceptance.Producer.RunID}] = true
			for _, route := range pin.Acceptance.Plan.Routes {
				result.Generations[route.ConfigGeneration] = true
			}
			after = pin.ID
		}
		if len(page) < 100 {
			break
		}
	}
	for after := (triggerqueue.EventRootDependency{}); ; {
		page, err := queue.EventRootDependencyPage(ctx, after, 100)
		if err != nil {
			return Dependencies{}, err
		}
		for _, pin := range page {
			result.Runs[RunRef{Gaggle: pin.Gaggle, RunID: pin.SourceRunID}] = true
			after = pin
		}
		if len(page) < 100 {
			return result, nil
		}
	}
}
