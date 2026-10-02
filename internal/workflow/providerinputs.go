package workflow

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
		inputs, known := providerstage.InputSchemaForVersion(command, def.DSLVersion)
		if !known {
			continue
		}
		declared := make(map[string]providerstage.Input, len(inputs))
		for _, input := range inputs {
			declared[input.Name] = input
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
		for _, name := range sortedProviderInputKeys(task.Inputs) {
			if _, ok := declared[name]; !ok {
				problems = append(problems, undeclaredProviderInputProblem(task.Name, "", command, name))
			}
		}
		for _, name := range sortedProviderInputKeys(task.InputsFrom) {
			if _, ok := declared[name]; !ok {
				problems = append(problems, undeclaredProviderInputProblem(task.Name, "", command, name))
			}
		}
		if task.Experiment != nil {
			for _, arm := range task.Experiment.Arms {
				for _, name := range sortedProviderInputKeys(arm.Variant) {
					if _, ok := declared[name]; !ok {
						problems = append(problems, undeclaredProviderInputProblem(task.Name, arm.Name, command, name))
					}
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

func undeclaredProviderInputProblem(task, arm, command, input string) string {
	location := fmt.Sprintf("task %q", task)
	if arm != "" {
		location += fmt.Sprintf(" experiment arm %q", arm)
	}
	return fmt.Sprintf("%s runs built-in `goobers %s` and sets undeclared input %q", location, command, input)
}

func sortedProviderInputKeys(inputs map[string]string) []string {
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CheckProviderStageUnsetDefaults reports built-in provider-stage inputs that
// carry a policy default (providerstage.Input.UnsetDefault) and that a task
// leaves unset. The stage runs on the default, so these are warnings, not
// errors: they make an implicit policy choice visible to the author (#2737).
// An input counts as set when the task supplies it through a non-empty
// literal or inputsFrom, or when every experiment arm supplies a non-empty
// variant value; an empty literal still runs on the default. A command-line
// override flag (Input.UnsetDefaultOverrideFlag) supersedes the default.
func CheckProviderStageUnsetDefaults(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
			continue
		}
		command := task.Run.Command[1]
		inputs, known := providerstage.InputSchemaForVersion(command, def.DSLVersion)
		if !known {
			continue
		}
		for _, input := range inputs {
			if input.UnsetDefault == "" || input.State == providerstage.InputRetired ||
				commandHasFlag(task.Run.Command[2:], input.UnsetDefaultOverrideFlag) ||
				taskSetsProviderInput(task, input.Name) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"task %q runs `goobers %s` without input %q; it defaults to %s — set it explicitly to choose this policy",
				task.Name, command, input.Name, input.UnsetDefault,
			))
		}
	}
	return problems
}

func commandHasFlag(args []string, flag string) bool {
	if flag == "" {
		return false
	}
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func taskSetsProviderInput(task apiv1.Task, name string) bool {
	if task.Inputs[name] != "" {
		return true
	}
	if _, ok := task.InputsFrom[name]; ok {
		return true
	}
	if task.Experiment == nil || len(task.Experiment.Arms) == 0 {
		return false
	}
	for _, arm := range task.Experiment.Arms {
		if arm.Variant[name] == "" {
			return false
		}
	}
	return true
}
