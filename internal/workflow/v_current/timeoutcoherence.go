package vcurrent

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
)

// CheckStageTimeoutCoherence reports statically bounded waits that can outlive
// the stage executing them. Dynamic duration inputs are left alone because
// their runtime value cannot be proven unsafe from the workflow definition.
func CheckStageTimeoutCoherence(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		if _, dynamicKind := task.InputsFrom[boundedwait.InputKind]; dynamicKind {
			continue
		}
		switch task.Inputs[boundedwait.InputKind] {
		case "", boundedwait.KindShell, boundedwait.KindCIPoll:
		default:
			continue
		}

		stageTimeout, ok := effectiveStageTimeout(task)
		if !ok {
			continue
		}
		wait, source, ok := boundedPollWait(task)
		if !ok {
			continue
		}
		wait, clamp, ok := effectivePollWait(task, wait, stageTimeout)
		if !ok || wait < stageTimeout {
			continue
		}
		if clamp != "" {
			source += ", " + clamp
		}
		problems = append(problems, fmt.Sprintf(
			"task %q inputs.%s has effective bounded wait %s (%s), meeting or exceeding its effective stage timeout %s; the executor can terminate the stage before the wait finishes and before the stage writes a result",
			task.Name, boundedwait.InputPollTimeout, wait, source, stageTimeout,
		))
	}
	return problems
}

func effectiveStageTimeout(task apiv1.Task) (time.Duration, bool) {
	limits := TaskLimits(task)
	if limits.MaxDurationSeconds > 0 {
		return time.Duration(limits.MaxDurationSeconds) * time.Second, true
	}
	if !isShellStage(task) {
		return 0, false
	}
	if _, dynamic := task.InputsFrom[boundedwait.InputTimeout]; dynamic {
		return 0, false
	}
	value := task.Inputs[boundedwait.InputTimeout]
	if value == "" {
		return boundedwait.DefaultTimeout, true
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, false
	}
	return timeout, true
}

func boundedPollWait(task apiv1.Task) (time.Duration, string, bool) {
	if _, dynamic := task.InputsFrom[boundedwait.InputPollTimeout]; dynamic {
		return 0, "", false
	}
	value := task.Inputs[boundedwait.InputPollTimeout]
	if value == "" {
		if !isCIPollStage(task) {
			subcommand, ok := goobersSubcommand(task)
			if !ok || subcommand != "merge-queue-poll" {
				return 0, "", false
			}
		}
		return boundedwait.DefaultPollTimeout, fmt.Sprintf("default %s", boundedwait.DefaultPollTimeout), true
	}
	wait, err := time.ParseDuration(value)
	if err != nil || wait <= 0 {
		return 0, "", false
	}
	return wait, fmt.Sprintf("declared %q", value), true
}

func effectivePollWait(task apiv1.Task, wait, stageTimeout time.Duration) (time.Duration, string, bool) {
	if isCIPollStage(task) {
		if budget := boundedwait.CIPollBudget(stageTimeout); wait > budget {
			return budget, fmt.Sprintf("clamped from %s by ci-poll", wait), true
		}
		return wait, "", true
	}
	subcommand, ok := goobersSubcommand(task)
	if !ok || subcommand != "merge-queue-poll" {
		return wait, "", true
	}
	clampStageTimeout, ok := mergeQueueCommandStageTimeout(task)
	if !ok {
		return 0, "", false
	}
	if budget := boundedwait.MergeQueuePollBudget(clampStageTimeout); wait > budget {
		return budget, fmt.Sprintf("clamped from %s by merge-queue-poll", wait), true
	}
	return wait, "", true
}

// mergeQueueCommandStageTimeout mirrors cmd/goobers.stageTimeout: the
// subprocess sees inputs.timeout, not the task's canonical timeoutSeconds.
func mergeQueueCommandStageTimeout(task apiv1.Task) (time.Duration, bool) {
	if _, dynamic := task.InputsFrom[boundedwait.InputTimeout]; dynamic {
		return 0, false
	}
	value := task.Inputs[boundedwait.InputTimeout]
	if timeout, err := time.ParseDuration(value); err == nil && timeout > 0 {
		return timeout, true
	}
	return boundedwait.DefaultTimeout, true
}

func isCIPollStage(task apiv1.Task) bool {
	return task.Type == apiv1.TaskDeterministic &&
		task.Inputs[boundedwait.InputKind] == boundedwait.KindCIPoll
}

// expectedSubprocessTimeoutInput is an optional stage annotation naming the
// wall-clock ceiling of a subprocess CheckSubprocessTimeoutCoherence cannot
// parse from the command itself — a wrapped tool that carries its own
// timeout. Authoring tools and reference workflows populate it so validate
// still catches the contradiction for a stage it cannot otherwise classify.
const expectedSubprocessTimeoutInput = "expectedSubprocessTimeoutSeconds"

// goTestTimeoutFlag is the flag `go test` reads for its own wall-clock
// ceiling.
const goTestTimeoutFlag = "-timeout"

// goTestTimeoutEnv is the Makefile variable (`GO_TEST_TIMEOUT ?= 30m`) this
// repo's `make` targets forward into `go test -timeout` (or an equivalent
// hardcoded ceiling — see test/ci/main.go). A stage that pins this in its own
// run.env is telling validate the exact ceiling its `make <target>`
// invocation carries.
const goTestTimeoutEnv = "GO_TEST_TIMEOUT"

// CheckSubprocessTimeoutCoherence reports a deterministic stage whose command
// wraps a subprocess carrying its own, longer wall-clock ceiling: the
// executor kills the stage before the subprocess's timeout can expire
// whenever the workload approaches it, so the stage is unwinnable by
// construction no matter how it performs on a typical run (#3377 — a stage
// budget below `make ci`'s `go test -race -timeout 30m` validated clean and
// then discarded 25 minutes of genuine progress live).
//
// Detection only trusts evidence visible in the stage's own declaration: a
// literal `go test -timeout` flag, an explicit GO_TEST_TIMEOUT override on a
// `make` invocation, or the expectedSubprocessTimeoutSeconds escape hatch for
// a wrapped tool this cannot parse. An unannotated `make <target>` with no
// declared override is left alone on purpose: assuming this repo's own
// GO_TEST_TIMEOUT default for every `make` invocation across every instance
// would misfire on a target repo whose toolchain is not even Go.
func CheckSubprocessTimeoutCoherence(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		stageTimeout, ok := effectiveStageTimeout(task)
		if !ok {
			continue
		}
		subTimeout, source, ok := discoverSubprocessTimeout(task)
		if !ok || subTimeout < stageTimeout {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"task %q stage budget %s cannot contain subprocess timeout %s (%s); the executor kills the stage before the subprocess's own ceiling expires whenever the workload approaches it, so the stage loses the race by construction",
			task.Name, stageTimeout, subTimeout, source,
		))
	}
	return problems
}

// discoverSubprocessTimeout returns the subprocess wall-clock ceiling a
// stage's command carries, when validate can tell from the stage's own
// declaration. The annotation is checked first: an author's explicit
// declaration always wins over a heuristic read of the command.
func discoverSubprocessTimeout(task apiv1.Task) (time.Duration, string, bool) {
	if timeout, ok := declaredSubprocessTimeout(task); ok {
		return timeout, fmt.Sprintf("declared %s=%ds", expectedSubprocessTimeoutInput, int64(timeout/time.Second)), true
	}
	if !isShellStage(task) {
		return 0, "", false
	}
	return commandSubprocessTimeout(task.Run)
}

// declaredSubprocessTimeout reads the expectedSubprocessTimeoutSeconds escape
// hatch. A dynamic value cannot be proven unsafe from the workflow
// definition, mirroring the dynamic guards elsewhere in this file.
func declaredSubprocessTimeout(task apiv1.Task) (time.Duration, bool) {
	if _, dynamic := task.InputsFrom[expectedSubprocessTimeoutInput]; dynamic {
		return 0, false
	}
	value := strings.TrimSpace(task.Inputs[expectedSubprocessTimeoutInput])
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// commandSubprocessTimeout parses a `go test -timeout` flag or a `make`
// target's explicit GO_TEST_TIMEOUT override from the stage's declared
// command. Only run.command is inspected — a shell run.script is not parsed,
// keeping detection precise over complete.
func commandSubprocessTimeout(run *apiv1.DeterministicRun) (time.Duration, string, bool) {
	if run == nil || len(run.Command) == 0 {
		return 0, "", false
	}
	cmd := run.Command
	switch cmd[0] {
	case "go":
		if len(cmd) < 2 || cmd[1] != "test" {
			return 0, "", false
		}
		raw, ok := flagValue(cmd[2:], goTestTimeoutFlag)
		if !ok {
			return 0, "", false
		}
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return 0, "", false
		}
		return timeout, fmt.Sprintf("go test %s %s", goTestTimeoutFlag, raw), true
	case "make":
		raw, ok := run.Env[goTestTimeoutEnv]
		if !ok {
			return 0, "", false
		}
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return 0, "", false
		}
		return timeout, fmt.Sprintf("make target's %s=%s override", goTestTimeoutEnv, raw), true
	default:
		return 0, "", false
	}
}

// flagValue returns the value of a `-flag value` or `-flag=value` argument.
func flagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			return v, true
		}
	}
	return "", false
}
