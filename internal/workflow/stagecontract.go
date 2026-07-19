package workflow

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Stage-contract analysis (issue #900). These checks target a specific,
// dangerous class of defect: a workflow that is structurally valid, compiles,
// passes every existing check, and then silently loses data at runtime.
//
// The motivating live failure: merge-review's elect-lander declared five
// expectedOutputs and no resultFile. A shell stage's Outputs are harvested
// ONLY from a declared result file, so it emitted none of them while still
// exiting 0 — its successor gate read the missing key as false, routed every
// needs-changes review down the wrong branch, and the stage after that died
// resolving an inputsFrom against the same empty outputs. That severed the
// only path from merge-review to pr-remediation and stalled the instance for
// three days, with no error until two stages downstream.
//
// Nothing caught it because expectedOutputs is documentation, not
// enforcement, and inputsFrom is resolved purely at runtime. Both checks
// below are static: they run at `goobers validate` time, for ANY instance's
// own workflows, not just the ones shipped here.

// CheckStageContracts reports stage output/input contract violations: a
// stage that promises outputs it cannot emit, and a stage that reads an
// upstream output the stage actually preceding it does not produce.
//
// It is a no-op on a structurally broken graph, matching CheckReachability:
// those problems are reported field-by-field by the validator, and walking a
// broken graph only cascades misleading messages.
func CheckStageContracts(def Definition) []string {
	m := newMachine(def)
	if len(structuralProblems(m)) > 0 {
		return nil
	}
	problems := undeclaredResultFileProblems(def, consumedOutputKeys(def), true)
	return append(problems, unsatisfiableInputsFromProblems(m)...)
}

// CheckStageContractWarnings reports the non-breaking half of the same
// analysis: a stage promising outputs it cannot emit that nothing downstream
// actually reads. Wrong, and worth fixing before something does start
// reading it — but not a runtime failure today. The moment any stage's
// inputsFrom references such a key it becomes an error via
// CheckStageContracts.
//
// Deliberately NOT wired into `goobers validate`: #881's VER003
// ("expectedOutputs is declared but not enforced") already warns on every
// stage this would flag, and two warnings for one missing line is noise.
// Exported for callers that want the strict bar anyway — this repo holds
// its own shipped workflows to it, since "nothing reads it yet" is one
// inputsFrom away from an outage.
func CheckStageContractWarnings(def Definition) []string {
	m := newMachine(def)
	if len(structuralProblems(m)) > 0 {
		return nil
	}
	return undeclaredResultFileProblems(def, consumedOutputKeys(def), false)
}

// consumedOutputKeys is every upstream output key some stage reads through
// inputsFrom. Membership is what separates "declares an output it cannot
// emit" (bad hygiene) from "a downstream stage will read nothing" (broken).
func consumedOutputKeys(def Definition) map[string]bool {
	consumed := map[string]bool{}
	for _, task := range def.Spec.Tasks {
		for _, outputKey := range task.InputsFrom {
			consumed[outputKey] = true
		}
	}
	return consumed
}

// undeclaredResultFileProblems reports shell stages promising outputs they
// have no channel to emit. A deterministic stage's Outputs are read from the
// path named by its resultFile input and nowhere else (internal/executor's
// shell executor performs the whole harvest inside `if resultFile != ""`),
// so expectedOutputs without resultFile is a guaranteed silent no-op — the
// stage still exits 0, and every downstream reader sees nothing.
//
// Deliberately scoped to SHELL stages. A deterministic stage declaring
// inputs.kind is dispatched to that built-in executor instead (kind=ci-poll
// goes to CIPollExecutor, which never shells out and produces its outputs
// directly), and agentic stages produce theirs by their own mechanism. Both
// legitimately declare expectedOutputs with no resultFile, so keying off
// "has a Run command" alone would report them as violations.
// wantConsumed selects which half to report: true yields the breaking cases
// (a promised-but-unemittable key that some stage reads), false yields the
// hygiene cases (nothing reads it yet).
func undeclaredResultFileProblems(def Definition, consumed map[string]bool, wantConsumed bool) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		// Empty or "shell" is the shell executor; anything else is a
		// built-in kind with its own output channel.
		if kind := strings.TrimSpace(task.Inputs["kind"]); kind != "" && kind != "shell" {
			continue
		}
		if len(task.ExpectedOutputs) == 0 {
			continue
		}
		if strings.TrimSpace(task.Inputs["resultFile"]) != "" {
			continue
		}
		var read []string
		for _, key := range task.ExpectedOutputs {
			if consumed[key] {
				read = append(read, key)
			}
		}
		if (len(read) > 0) != wantConsumed {
			continue
		}
		if wantConsumed {
			problems = append(problems, fmt.Sprintf(
				"task %q declares expectedOutputs %v but no inputs.resultFile, and %v %s read downstream through inputsFrom; a deterministic stage's outputs are read only from its declared result file, so it will emit none of them, still exit 0, and the reader will fail",
				task.Name, task.ExpectedOutputs, read, plural(len(read), "is", "are"),
			))
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"task %q declares expectedOutputs %v but no inputs.resultFile; a deterministic stage's outputs are read only from its declared result file, so it emits none of them. Nothing reads them today, so this is not yet a failure — it becomes one the moment any stage's inputsFrom references one",
			task.Name, task.ExpectedOutputs,
		))
	}
	return problems
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// unsatisfiableInputsFromProblems reports inputsFrom references that cannot
// resolve at runtime. inputsFrom is a SINGLE-HOP handoff: it reads only the
// immediately preceding task's Outputs (gates are transparent to it), so a
// reference is satisfiable only if EVERY task that can immediately precede
// this one emits that key. A stage reachable by two branches whose other
// branch's predecessor does not emit it fails only on that branch — which is
// how these defects reach production having passed every test that exercised
// the happy path.
//
// Conservative by construction: expectedOutputs is not required to be
// exhaustive (shipped workflows deliberately omit conditionally-emitted keys
// such as landOutcome), so a predecessor declaring NO expectedOutputs is
// treated as unknown rather than as a violation. Only a predecessor that
// declares a set, and omits the referenced key from it, is reported.
func unsatisfiableInputsFromProblems(m *Machine) []string {
	var problems []string
	for _, name := range sortedTaskNames(m) {
		task := m.tasks[name]
		if len(task.InputsFrom) == 0 {
			continue
		}
		preceding := precedingTasks(m, name)
		for _, inputKey := range sortedKeys(task.InputsFrom) {
			outputKey := task.InputsFrom[inputKey]
			for _, predName := range preceding {
				pred := m.tasks[predName]
				if len(pred.ExpectedOutputs) == 0 {
					continue
				}
				if containsString(pred.ExpectedOutputs, outputKey) {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"task %q reads inputsFrom %q from upstream output %q, but on the path through task %q that stage declares outputs %v and not %q; inputsFrom resolves against the immediately preceding task only, so this branch fails at runtime",
					name, inputKey, outputKey, predName, pred.ExpectedOutputs, outputKey,
				))
			}
		}
	}
	return problems
}

// precedingTasks returns every task that can immediately precede target at
// runtime. Gates are transparent to inputsFrom — the runner carries the last
// TASK's result across them — so the walk continues back through any chain of
// gates until it reaches tasks. Cycles (a gate routing back to an earlier
// state) terminate on the visited set.
func precedingTasks(m *Machine, target string) []string {
	incoming := map[string][]string{}
	for _, state := range allStateNames(m) {
		for _, next := range m.outgoing(state) {
			incoming[next] = append(incoming[next], state)
		}
	}
	visited := map[string]bool{}
	var tasks []string
	var walk func(string)
	walk = func(state string) {
		for _, prev := range incoming[state] {
			if visited[prev] {
				continue
			}
			visited[prev] = true
			if _, isTask := m.tasks[prev]; isTask {
				tasks = append(tasks, prev)
				continue
			}
			walk(prev)
		}
	}
	walk(target)
	sort.Strings(tasks)
	return tasks
}

func allStateNames(m *Machine) []string {
	names := make([]string, 0, len(m.tasks)+len(m.gates))
	for name := range m.tasks {
		names = append(names, name)
	}
	for name := range m.gates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTaskNames(m *Machine) []string {
	names := make([]string, 0, len(m.tasks))
	for name := range m.tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
