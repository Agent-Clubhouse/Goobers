package readservice

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

type calibrationReader interface {
	HarvestCalibration(context.Context, time.Time, time.Time, int) (readmodel.CalibrationSnapshot, error)
}

// SimulateWorkflow calibrates a workflow from the attached read model and
// returns a reproducible, read-only projection.
func (s *Local) SimulateWorkflow(ctx context.Context, definition vnext.Definition,
	windowStart, windowEnd time.Time, options vnext.SimulationOptions) (vnext.SimulationResult, error) {
	source, ok := s.sources.ReadModel.(calibrationReader)
	if !ok {
		return vnext.SimulationResult{}, fmt.Errorf("read service: calibration read model is unavailable")
	}
	snapshot, err := source.HarvestCalibration(ctx, windowStart, windowEnd, 0)
	if err != nil {
		return vnext.SimulationResult{}, err
	}
	calibration := vnext.Calibration{
		WindowStart: snapshot.WindowStart,
		WindowEnd:   snapshot.WindowEnd,
		Runs:        snapshot.Runs,
		MinSamples:  snapshot.MinSamples,
		Outcomes:    snapshot.Outcomes,
		Gates:       snapshot.Gates,
		Nodes:       make(map[string]vnext.NodeCalibration, len(snapshot.Nodes)),
	}
	for name, observation := range snapshot.Nodes {
		calibration.Nodes[name] = vnext.NodeCalibration{
			Samples:    observation.Samples,
			Successes:  observation.Successes,
			Durations:  observation.Durations,
			RetryWaste: observation.RetryWaste,
			Costs:      observation.Costs,
		}
	}

	return vnext.Simulate(definition, calibration, options)
}

// EvaluateWorkflowWhatIf compares a baseline definition with a proposed
// candidate using one journal-derived calibration snapshot.
func (s *Local) EvaluateWorkflowWhatIf(ctx context.Context, baseline, candidate vnext.Definition,
	windowStart, windowEnd time.Time, options vnext.SimulationOptions) (vnext.WhatIfResult, error) {
	source, ok := s.sources.ReadModel.(calibrationReader)
	if !ok {
		return vnext.WhatIfResult{}, fmt.Errorf("read service: calibration read model is unavailable")
	}
	snapshot, err := source.HarvestCalibration(ctx, windowStart, windowEnd, 0)
	if err != nil {
		return vnext.WhatIfResult{}, err
	}
	calibration := vnext.Calibration{
		WindowStart: snapshot.WindowStart, WindowEnd: snapshot.WindowEnd,
		Runs: snapshot.Runs, MinSamples: snapshot.MinSamples,
		Outcomes: snapshot.Outcomes, Gates: snapshot.Gates,
		Nodes: make(map[string]vnext.NodeCalibration, len(snapshot.Nodes)),
	}
	for name, observation := range snapshot.Nodes {
		calibration.Nodes[name] = vnext.NodeCalibration{
			Samples: observation.Samples, Successes: observation.Successes,
			Durations: observation.Durations, RetryWaste: observation.RetryWaste,
			Costs: observation.Costs,
		}
	}
	baseGraph, problems := vnext.GraphForDefinition(baseline)
	if len(problems) != 0 {
		return vnext.WhatIfResult{}, fmt.Errorf("read service: baseline workflow: %s", problems[0])
	}
	candidateGraph, problems := vnext.GraphForDefinition(candidate)
	if len(problems) != 0 {
		return vnext.WhatIfResult{}, fmt.Errorf("read service: candidate workflow: %s", problems[0])
	}
	return vnext.EvaluateWhatIf(baseGraph, candidateGraph, calibration, options)
}
