package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
	"github.com/goobers/goobers/internal/providerstage"
)

// CheckStageDurationInputs reports static duration-typed stage inputs that
// time.ParseDuration rejects (#7068). The executors and built-in commands
// parse these values only at run time, so without this check a value such as
// pollTimeoutSeconds: "3300" validates cleanly and then fails every run.
//
// Every static input typed as a duration by the executor the stage's kind
// selects is checked, even where the runtime may later override it (a task
// duration ceiling shadowing inputs.timeout, gate cadence replacing a ci-poll
// pollIntervalSeconds). Inputs supplied
// through inputsFrom replace any literal or experiment variant at dispatch,
// so they are left to the runtime parsers. When inputsFrom supplies kind
// itself, every executor the stage may resolve to is considered. A task with
// an experiment always runs one of its arms, so each arm is checked against
// its effective inputs (base inputs overlaid by the arm's variant), mirroring
// the order the runner applies them; a base value every arm replaces never
// reaches an executor.
func CheckStageDurationInputs(def Definition) []string {
	if _, err := interpreterForDefinition(def); err != nil {
		// The unsupported DSL version is reported by the other facades.
		return nil
	}
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		problems = append(problems, taskDurationInputProblems(task, def.DSLVersion)...)
	}
	return problems
}

func taskDurationInputProblems(task apiv1.Task, dslVersion string) []string {
	if task.Experiment == nil || len(task.Experiment.Arms) == 0 {
		var problems []string
		for _, name := range parsedDurationInputs(task, task.Inputs, dslVersion) {
			problems = appendDurationInputProblem(problems, task.Name, "", name, task.Inputs[name])
		}
		return problems
	}
	// Base values are reported once, without an arm, when any arm passes
	// them through to a parser; variant values are attributed to their arm.
	var baseProblems, armProblems []string
	var baseReported []string
	for _, arm := range task.Experiment.Arms {
		effective := maps.Clone(task.Inputs)
		if effective == nil {
			effective = map[string]string{}
		}
		maps.Copy(effective, arm.Variant)
		for _, name := range parsedDurationInputs(task, effective, dslVersion) {
			if _, fromVariant := arm.Variant[name]; fromVariant {
				armProblems = appendDurationInputProblem(armProblems, task.Name, arm.Name, name, effective[name])
			} else if !slices.Contains(baseReported, name) {
				baseReported = append(baseReported, name)
				baseProblems = appendDurationInputProblem(baseProblems, task.Name, "", name, effective[name])
			}
		}
	}
	return append(baseProblems, armProblems...)
}

// parsedDurationInputs names the inputs, among effective, that the executor
// selected by effective's kind parses as durations: the ci-poll executor's
// poll cadence and budget, the external-telemetry executor's window,
// freshness and query limits, or the shell executor's timeout plus any
// duration declared by a built-in provider command. When inputsFrom supplies kind, the
// executor is unknown until dispatch, so the candidates of every executor it
// may resolve to are checked. Values the runtime may override or ignore (a
// timeout behind a task duration ceiling, a ci-poll interval replaced by gate
// cadence) are still duration-typed and must parse.
func parsedDurationInputs(task apiv1.Task, effective map[string]string, dslVersion string) []string {
	var candidates []string
	if _, dynamicKind := task.InputsFrom[boundedwait.InputKind]; dynamicKind {
		candidates = append(boundedwait.CIPollDurationInputs(), boundedwait.ExternalTelemetryDurationInputs()...)
		candidates = append(candidates, shellDurationCandidates(task, dslVersion)...)
	} else {
		switch effective[boundedwait.InputKind] {
		case boundedwait.KindCIPoll:
			candidates = boundedwait.CIPollDurationInputs()
		case boundedwait.KindExternalTelemetry:
			candidates = boundedwait.ExternalTelemetryDurationInputs()
		case "", boundedwait.KindShell:
			candidates = shellDurationCandidates(task, dslVersion)
		}
	}
	var names []string
	for _, name := range candidates {
		if _, dynamic := task.InputsFrom[name]; dynamic || effective[name] == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func shellDurationCandidates(task apiv1.Task, dslVersion string) []string {
	candidates := []string{boundedwait.InputTimeout}
	if len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return candidates
	}
	inputs, _ := providerstage.InputSchemaForVersion(task.Run.Command[1], dslVersion)
	for _, input := range inputs {
		if input.Type == providerstage.InputDuration && input.State == providerstage.InputCurrent &&
			input.Name != boundedwait.InputTimeout {
			candidates = append(candidates, input.Name)
		}
	}
	return candidates
}

func appendDurationInputProblem(problems []string, task, arm, name, value string) []string {
	location := fmt.Sprintf("task %q inputs.%s", task, name)
	if arm != "" {
		location = fmt.Sprintf("task %q experiment arm %q input %s", task, arm, name)
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		if parsed > 0 || !slices.Contains(boundedwait.ExternalTelemetryDurationInputs(), name) {
			return problems
		}
		return append(problems, fmt.Sprintf("%s %q must be a positive duration", location, value))
	}
	return append(problems, fmt.Sprintf(
		"%s %q is not a valid duration; %s",
		location, value, durationInputHint(value),
	))
}

// durationInputHint suggests a corrected spelling. A bare integer is the
// common mistake because several duration inputs end in "Seconds".
func durationInputHint(value string) string {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 &&
		seconds <= int64(time.Duration(1<<63-1)/time.Second) {
		return fmt.Sprintf(
			"duration inputs need a unit despite any \"Seconds\" suffix in the name: use %q (%s)",
			fmt.Sprintf("%ds", seconds), time.Duration(seconds)*time.Second,
		)
	}
	return `use a Go duration with a unit, such as "90s", "15m", or "1h30m"`
}
