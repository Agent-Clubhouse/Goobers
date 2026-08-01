package readservice

import (
	"context"

	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// The telemetry adapter for the read model's measurement flags (#1782).
//
// This is the ONE place that knows how a stage's attempts turn into the four
// population predicates. It used to be evaluated per candidate inside an HTTP
// request (matchesTelemetryAttempts, called from the uncapped candidate loop);
// the logic is unchanged, but it now runs once per run projection and its answer
// is stored.

// stageAttemptSource is the narrow slice of telemetry this adapter needs.
//
// Narrowed to one method so a test can supply attempts without standing up a
// rollup store, and so it is obvious that projection reads telemetry for exactly
// one thing.
type stageAttemptSource interface {
	StageAttempts(ctx context.Context, runID string) ([]rollup.StageAttempt, error)
}

// TelemetryMeasurement derives read-model measurement flags from the telemetry
// rollup.
type TelemetryMeasurement struct{ source stageAttemptSource }

// NewTelemetryMeasurement adapts a telemetry source for the read model.
//
// Returns nil for a nil source rather than a wrapper around nothing: the read
// model treats a nil MeasurementSource as "no measurement available", and a
// non-nil wrapper that always errors would instead look like a source that is
// permanently failing.
func NewTelemetryMeasurement(source stageAttemptSource) readmodel.MeasurementSource {
	if source == nil {
		return nil
	}
	return &TelemetryMeasurement{source: source}
}

// RunMeasurement returns one StageMeasurement per stage that has attempts.
//
// Returns nil (not an empty slice) on error, which the read model reads as
// "nothing to say" and which preserves any flags already projected. A run with
// genuinely no attempts returns an empty non-nil slice, which clears them —
// the distinction is what stops a telemetry outage from silently emptying every
// population filter.
func (m *TelemetryMeasurement) RunMeasurement(ctx context.Context, runID string) ([]readmodel.StageMeasurement, error) {
	attempts, err := m.source.StageAttempts(ctx, runID)
	if err != nil {
		return nil, err
	}
	return measurementFromAttempts(attempts), nil
}

// measurementFromAttempts folds a run's attempts into per-stage predicates.
//
// # Why retry waste is computed per (stage, branch), not per stage
//
// An attempt is wasted when a LATER traversal of the same branch re-ran it, so
// the comparison is against the maximum traversal for that exact key. Comparing
// against the run's maximum would flag every stage in a run that repassed
// anywhere — which is a different, much broader population than the one the
// filter names.
//
// This mirrors matchesTelemetryAttempts exactly, which is the point: the read
// path's answer must not change because the evaluation moved.
func measurementFromAttempts(attempts []rollup.StageAttempt) []readmodel.StageMeasurement {
	type branchKey struct {
		stage       string
		branch      int
		branchKnown bool
	}
	latest := make(map[branchKey]int, len(attempts))
	for _, attempt := range attempts {
		key := branchKey{attempt.Stage, attempt.Branch, attempt.BranchKnown}
		if attempt.Traversal > latest[key] {
			latest[key] = attempt.Traversal
		}
	}

	byStage := make(map[string]*readmodel.StageMeasurement, len(attempts))
	order := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		row, seen := byStage[attempt.Stage]
		if !seen {
			row = &readmodel.StageMeasurement{Stage: attempt.Stage}
			byStage[attempt.Stage] = row
			order = append(order, attempt.Stage)
		}
		// Each flag is an OR across the stage's attempts: the filter asks whether
		// the stage has ANY attempt with the figure recorded, matching what the
		// per-candidate check returned true on.
		if attempt.InputTokens != nil && attempt.OutputTokens != nil {
			row.TokenMeasured = true
		}
		if attempt.CopilotPremiumRequests != nil {
			row.PremiumMeasured = true
		}
		if attempt.CostUSD != nil {
			row.CostMeasured = true
		}
		if attempt.Traversal < latest[branchKey{attempt.Stage, attempt.Branch, attempt.BranchKnown}] {
			row.RetryWaste = true
		}
	}

	// Non-nil even when empty: an empty result is a positive statement that this
	// run has no measured stages, and the read model clears flags on it.
	out := make([]readmodel.StageMeasurement, 0, len(order))
	for _, stage := range order {
		out = append(out, *byStage[stage])
	}
	return out
}
