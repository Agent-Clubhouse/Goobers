package v20

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func calibratedSimulationDefinition() Definition {
	return Definition{Name: "simulation", Spec: apiv1.WorkflowSpec{
		Start: "check",
		Tasks: []apiv1.Task{{
			Name: "check", Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"check"}},
			Next: "decision",
		}},
		Gates: []apiv1.Gate{{
			Name: "decision", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "x"},
			Branches:  map[string]string{"pass": TerminalComplete, "fail": TargetAbort},
		}},
	}}
}

func TestSimulateIsSeededAndReportsCalibration(t *testing.T) {
	calibration := Calibration{
		WindowStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Runs:        100, MinSamples: 2,
		Gates: map[string]map[string]int{"decision": {"pass": 3, "fail": 1}},
		Nodes: map[string]NodeCalibration{"check": {
			Samples: 4, Durations: []time.Duration{time.Second}, Costs: []float64{2},
		}},
	}
	first, err := Simulate(calibratedSimulationDefinition(), calibration, SimulationOptions{Samples: 200, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Simulate(calibratedSimulationDefinition(), calibration, SimulationOptions{Samples: 200, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcomes["complete"] != second.Outcomes["complete"] ||
		first.ExpectedCycleTime != second.ExpectedCycleTime {
		t.Fatalf("same seed produced different results: %#v %#v", first, second)
	}
	if first.Confidence != ConfidenceHigh || first.ExpectedCycleTime != time.Second ||
		first.ExpectedCost != 2 {
		t.Fatalf("result = %#v, want calibrated metadata and averages", first)
	}
}

func TestSimulateFallsBackForThinDataAndLabelsChangedNode(t *testing.T) {
	calibration := Calibration{
		Runs: 1, MinSamples: 2,
		Gates: map[string]map[string]int{"decision": {"pass": 1}},
	}
	result, err := Simulate(calibratedSimulationDefinition(), calibration, SimulationOptions{
		Samples: 10, Seed: 1,
		ChangedNodes: map[string]ChangedNode{"check": {Duration: 2 * time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FallbackNodes) != 0 || len(result.DistributionShift) != 1 ||
		result.DistributionShift[0] != "check" {
		t.Fatalf("provenance = %#v, want explicit shift without historical fallback", result)
	}
}

func TestSimulateThinDataGateFallbackExploresAllOutcomes(t *testing.T) {
	calibration := Calibration{
		Runs: 1, MinSamples: 10,
		Gates: map[string]map[string]int{"decision": {"pass": 1}},
	}
	result, err := Simulate(calibratedSimulationDefinition(), calibration, SimulationOptions{
		Samples: 200, Seed: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FallbackMode != "static-all-possible" {
		t.Fatalf("fallback mode = %q, want static-all-possible", result.FallbackMode)
	}
	if result.Outcomes["complete"] == 0 || result.Outcomes["abort"] == 0 {
		t.Fatalf("thin-data gate fallback = %#v, want both outcomes explored", result.Outcomes)
	}
}

func TestSimulateChangedNodeZeroSuccessProbabilityAlwaysFails(t *testing.T) {
	calibration := Calibration{
		Runs:       20,
		MinSamples: 2,
	}
	result, err := Simulate(calibratedSimulationDefinition(), calibration, SimulationOptions{
		Samples: 25, Seed: 3,
		ChangedNodes: map[string]ChangedNode{
			"check": {SuccessProbability: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcomes["abort"] != 25 || result.Outcomes["complete"] != 0 {
		t.Fatalf("outcomes = %#v, want all samples aborted", result.Outcomes)
	}
}
