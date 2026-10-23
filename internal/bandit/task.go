package bandit

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ConfigForTask validates the declared experiment against its actual next gate.
func ConfigForTask(machine interface {
	Gate(string) (apiv1.Gate, bool)
}, task apiv1.Task) (Config, bool, error) {
	if task.Experiment == nil {
		return Config{}, false, nil
	}
	e := task.Experiment
	arms := make([]Arm, len(e.Arms))
	for i, arm := range e.Arms {
		arms[i] = Arm{Name: arm.Name, Variant: arm.Variant, GateLevel: arm.GateLevel}
	}
	defaultArm, err := defaultBanditArm(arms)
	if err != nil {
		return Config{}, false, fmt.Errorf("task %q experiment: %w", task.Name, err)
	}
	if e.DefaultGateLevel != 0 && e.DefaultGateLevel != defaultArm.GateLevel {
		return Config{}, false, fmt.Errorf("task %q experiment defaultGateLevel (%d) must match default arm %q gateLevel (%d)",
			task.Name, e.DefaultGateLevel, defaultArm.Name, defaultArm.GateLevel)
	}

	// Validate that the task transitions to an actual gate in the workflow.
	if task.Next == "" {
		return Config{}, false, fmt.Errorf("task %q with experiment must transition to a gate (Next field is empty)", task.Name)
	}
	gate, ok := machine.Gate(task.Next)
	if !ok {
		return Config{}, false, fmt.Errorf("task %q experiment: task transitions to %q but no such gate exists in workflow", task.Name, task.Next)
	}

	actualGateLevel, err := banditGateLevel(gate)
	if err != nil {
		return Config{}, false, fmt.Errorf("task %q experiment: %w", task.Name, err)
	}
	if defaultArm.GateLevel != actualGateLevel {
		return Config{}, false, fmt.Errorf("task %q experiment control arm %q gateLevel (%d) must match actual gate %q level (%d)",
			task.Name, defaultArm.Name, defaultArm.GateLevel, gate.Name, actualGateLevel)
	}

	// Validate that all arms have a gate level not lower than the actual gate.
	for _, arm := range arms {
		if arm.GateLevel < actualGateLevel {
			return Config{}, false, fmt.Errorf("task %q experiment arm %q has gateLevel %d but actual gate %q has level %d (experimental arms must not weaken the gate)",
				task.Name, arm.Name, arm.GateLevel, gate.Name, actualGateLevel)
		}
	}

	return Config{Stage: task.Name, Seed: e.Seed, Arms: arms,
		ExplorationBudget: e.ExplorationBudget, MinSamples: e.MinSamples,
		MaxFailureRate: e.MaxFailureRate, MinLift: e.MinLift, Confidence: e.Confidence,
		TrainWindow: e.TrainWindow, EvalWindow: e.EvalWindow,
		DefaultGateLevel: defaultArm.GateLevel}, true, nil
}

func banditGateLevel(gate apiv1.Gate) (int, error) {
	switch gate.Evaluator {
	case apiv1.EvaluatorAutomated:
		return 1, nil
	case apiv1.EvaluatorAgentic:
		return 2, nil
	case apiv1.EvaluatorHuman:
		return 3, nil
	default:
		return 0, fmt.Errorf("gate %q has unsupported evaluator %q", gate.Name, gate.Evaluator)
	}
}

func defaultBanditArm(arms []Arm) (Arm, error) {
	if len(arms) == 0 {
		return Arm{}, fmt.Errorf("bandit requires at least one arm")
	}
	defaultArm := arms[0]
	for _, arm := range arms {
		if arm.Name == "control" {
			defaultArm = arm
			break
		}
	}
	return defaultArm, nil
}

// ObservationWindow selects the training or held-out evaluation window.
func ObservationWindow(config Config, observations []Observation) string {
	train := 0
	for _, observation := range observations {
		if observation.Stage == config.Stage && observation.Window == "train" {
			train++
		}
	}
	if train < config.TrainWindow {
		return "train"
	}
	return "eval"
}

func recordBanditOutcome(config Config, runID string, assignment Assignment, window string, result apiv1.ResultEnvelope, out Journal) (Observation, error) {
	success := result.Status == apiv1.ResultSuccess
	reward := 0.0
	if success {
		reward = 1
	}
	observation := Observation{
		Stage: config.Stage, RunID: runID, Arm: assignment.Arm,
		Reward: reward, RewardSet: true, Success: success,
		Window: window, Assigned: assignment.Seed,
	}
	return observation, config.RecordObservation(observation, out)
}

// RecordResult records an attempt outcome and any retirement or promotion proposal.
func RecordResult(config Config, runID string, assignment Assignment, window string, historical []Observation, result apiv1.ResultEnvelope, out Journal) error {
	observation, err := recordBanditOutcome(config, runID, assignment, window, result, out)
	if err != nil {
		return err
	}
	_, _, err = config.EvaluateAndRecord(append(historical, observation), out)
	return err
}
