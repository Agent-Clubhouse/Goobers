package readservice

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func unavailableTerminalSummary(status string) *readmodel.TerminalSummary {
	return (readmodel.TerminalFacts{}).Summary(journal.RunPhase(status), nil)
}

// withTerminalSummaries decorates the already selected telemetry items from
// the same persisted facts as ListRuns. Never open per-item journals: missing
// projections remain explicitly unavailable, including rollup-only readers.
func (s *Local) withTerminalSummaries(ctx context.Context, result TelemetryImplementationOutcomesResult) (TelemetryImplementationOutcomesResult, error) {
	if s.sources.ReadModel == nil || !s.readModelReads {
		return result, nil
	}
	for i := range result.Items {
		item := &result.Items[i]
		row, found, err := s.sources.ReadModel.GetRun(ctx, item.RunID)
		if err != nil {
			return TelemetryImplementationOutcomesResult{}, err
		}
		// Independent stores can straddle a resume. Do not combine the old
		// terminal rollup with a different lifecycle's projected facts.
		if !found || string(row.Phase) != item.Status || row.FinishedAt == nil || !row.FinishedAt.Equal(item.FinishedAt) {
			continue
		}
		item.TerminalSummary = row.Operator.Terminal.Summary(row.Phase, row.Operator.Reliability.TerminalCause)
	}
	return result, nil
}
