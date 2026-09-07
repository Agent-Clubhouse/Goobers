package main

import (
	"context"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

func (s *runnerFallbackStarter) observeFallback(ctx context.Context, req localscheduler.StartRequest) {
	if s.telemetry == nil {
		return
	}
	_, span, err := s.telemetry.StartSchedulerSpan(ctx, telemetry.SchedulerAttributes{
		Gaggle: req.Gaggle, WorkflowID: s.workflow, RunID: req.RunID,
		Action: engineStarterSelectionKind,
	})
	if err != nil {
		return // Observability must not alter dispatch admission or results.
	}
	span.SetEngineFallback(s.selection.ReasonClass, s.selection.PlacementDeclared, len(s.selection.SelfPinnedStages), len(s.selection.UnpinnedGates))
	span.End()
}
