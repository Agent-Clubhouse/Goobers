package readservice

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
)

type calibrationReader interface {
	HarvestCalibration(context.Context, time.Time, time.Time, int) (readmodel.CalibrationSnapshot, error)
}

// SimulateWorkflow calibrates a workflow from the attached read model and
// returns a reproducible, read-only projection.
func (s *Local) SimulateWorkflow(ctx context.Context, definition v20.Definition,
	windowStart, windowEnd time.Time, options v20.SimulationOptions) (v20.SimulationResult, error) {
	source, ok := s.sources.ReadModel.(calibrationReader)
	if !ok {
		return v20.SimulationResult{}, fmt.Errorf("read service: calibration read model is unavailable")
	}
	snapshot, err := source.HarvestCalibration(ctx, windowStart, windowEnd, 0)
	if err != nil {
		return v20.SimulationResult{}, err
	}
	calibration := v20.Calibration{
		WindowStart: snapshot.WindowStart,
		WindowEnd:   snapshot.WindowEnd,
		Runs:        snapshot.Runs,
		MinSamples:  snapshot.MinSamples,
		Outcomes:    snapshot.Outcomes,
		Gates:       snapshot.Gates,
		Nodes:       make(map[string]v20.NodeCalibration, len(snapshot.Nodes)),
	}
	for name, observation := range snapshot.Nodes {
		calibration.Nodes[name] = v20.NodeCalibration{
			Samples:    observation.Samples,
			Successes:  observation.Successes,
			Durations:  observation.Durations,
			RetryWaste: observation.RetryWaste,
			Costs:      observation.Costs,
		}
	}

	return v20.Simulate(definition, calibration, options)
}

// EvaluateWorkflowWhatIf compares a baseline definition with a proposed
// candidate using one journal-derived calibration snapshot.
func (s *Local) EvaluateWorkflowWhatIf(ctx context.Context, baseline, candidate v20.Definition,
	windowStart, windowEnd time.Time, options v20.SimulationOptions) (v20.WhatIfResult, error) {
	source, ok := s.sources.ReadModel.(calibrationReader)
	if !ok {
		return v20.WhatIfResult{}, fmt.Errorf("read service: calibration read model is unavailable")
	}
	snapshot, err := source.HarvestCalibration(ctx, windowStart, windowEnd, 0)
	if err != nil {
		return v20.WhatIfResult{}, err
	}
	calibration := v20.Calibration{
		WindowStart: snapshot.WindowStart, WindowEnd: snapshot.WindowEnd,
		Runs: snapshot.Runs, MinSamples: snapshot.MinSamples,
		Outcomes: snapshot.Outcomes, Gates: snapshot.Gates,
		Nodes: make(map[string]v20.NodeCalibration, len(snapshot.Nodes)),
	}
	for name, observation := range snapshot.Nodes {
		calibration.Nodes[name] = v20.NodeCalibration{
			Samples: observation.Samples, Successes: observation.Successes,
			Durations: observation.Durations, RetryWaste: observation.RetryWaste,
			Costs: observation.Costs,
		}
	}
	baseGraph, problems := v20.GraphForDefinition(baseline)
	if len(problems) != 0 {
		return v20.WhatIfResult{}, fmt.Errorf("read service: baseline workflow: %s", problems[0])
	}
	candidateGraph, problems := v20.GraphForDefinition(candidate)
	if len(problems) != 0 {
		return v20.WhatIfResult{}, fmt.Errorf("read service: candidate workflow: %s", problems[0])
	}
	return v20.EvaluateWhatIf(baseGraph, candidateGraph, calibration, options)
}
