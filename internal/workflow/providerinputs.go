package workflow

import (
	"fmt"

	"github.com/goobers/goobers/internal/providerstage"
)

// CheckProviderStageInputs reports workflow inputs whose built-in provider
// command has retired them. This check deliberately runs outside the
// versioned interpreters: a retirement recorded here matches a runtime defense
// in the current binary and therefore applies to every DSL version that binary
// can execute.
func CheckProviderStageInputs(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
			continue
		}
		command := task.Run.Command[1]
		for _, input := range providerstage.InputsForVersion(command, def.DSLVersion) {
			if input.State != providerstage.InputRetired {
				continue
			}
			_, literal := task.Inputs[input.Name]
			_, dynamic := task.InputsFrom[input.Name]
			if literal || dynamic {
				problems = append(problems, retiredProviderInputProblem(task.Name, "", command, input))
			}
			if task.Experiment == nil {
				continue
			}
			for _, arm := range task.Experiment.Arms {
				if _, set := arm.Variant[input.Name]; set {
					problems = append(problems, retiredProviderInputProblem(task.Name, arm.Name, command, input))
				}
			}
		}
	}
	return problems
}

func retiredProviderInputProblem(task, arm, command string, input providerstage.Input) string {
	location := fmt.Sprintf("task %q", task)
	if arm != "" {
		location += fmt.Sprintf(" experiment arm %q", arm)
	}
	return fmt.Sprintf(
		"%s runs `goobers %s` and sets retired input %q (retired since %s); %s",
		location, command, input.Name, input.RetiredSince, input.Replacement,
	)
}
