package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// telemetryevidence.go is where a CLI stage gets the derived telemetry
// evidence it used to read straight off the instance's rollup file (decision
// 005 R4 / finding 002 C3). Two constructions, selected exactly the way
// claimledger.go selects a claim ledger:
//
//   - the telemetry read plane when the stage's environment names it (a stage
//     pod), fail closed in between — an endpoint with no bearer or no gaggle
//     is an error, never a silent fall-through to a rollup file the pod does
//     not have and would read as "no evidence";
//   - the instance's own rollup under the instance root otherwise, byte for
//     byte the read that lived inline in applyImplementationFeedback before
//     this file existed, including its treatment of a missing or empty
//     database as "no evidence yet" rather than a failure.
//
// Only DERIVED, low-sensitivity projections travel this way: this evidence,
// and the four defect-nomination aggregates `telemetry-query` reads since
// Goobers#4001 (telemetrydefectplane.go). EXTERNAL telemetry connectors
// (executor.KindExternalTelemetry) reach a third-party vendor with the
// instance's own credential rather than this rollup, have no plane, and stay
// refused on the engine path.

// stageImplementationOutcomes returns the terminal implementation runs that
// claimed a backlog item in this stage's gaggle since the given instant.
func stageImplementationOutcomes(ctx context.Context, root string, since time.Time) ([]rollup.ImplementationOutcome, error) {
	plane, selected, err := telemetryclient.Select(os.Getenv)
	if err != nil {
		return nil, err
	}
	if selected {
		items, err := plane.ImplementationOutcomes(ctx, since)
		if err != nil {
			return nil, err
		}
		outcomes := make([]rollup.ImplementationOutcome, 0, len(items))
		for _, item := range items {
			outcomes = append(outcomes, rollup.ImplementationOutcome{
				RunID:        item.RunID,
				ItemID:       item.ItemID,
				Status:       item.Status,
				StartedAt:    item.StartedAt,
				FinishedAt:   item.FinishedAt,
				Stage:        item.Stage,
				ErrorCode:    item.ErrorCode,
				ErrorMessage: item.ErrorMessage,
				Gate:         item.Gate,
				Verdict:      item.Verdict,
			})
		}
		return outcomes, nil
	}
	return localImplementationOutcomes(ctx, root, since)
}

// localImplementationOutcomes is the instance-root read. A rollup that does
// not exist yet, or exists but is empty, is no evidence — not an error: an
// instance that has never finished an implementation run has nothing to feed
// back, and that was true before the plane existed too.
func localImplementationOutcomes(ctx context.Context, root string, since time.Time) ([]rollup.ImplementationOutcome, error) {
	dbPath := layoutFor(root).TelemetryDB()
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect telemetry rollup %s: %w", dbPath, err)
	}
	if info.Size() == 0 {
		return nil, nil
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open telemetry rollup %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	outcomes, err := db.ImplementationOutcomes(ctx, providerGaggle(), since)
	if err != nil {
		return nil, fmt.Errorf("query implementation outcomes: %w", err)
	}
	return outcomes, nil
}
