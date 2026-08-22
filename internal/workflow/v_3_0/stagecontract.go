package v30

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
	m, buildProblems := newMachineForCheck(def)
	if len(buildProblems) > 0 {
		return buildProblems
	}
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
	m, buildProblems := newMachineForCheck(def)
	if len(buildProblems) > 0 {
		return buildProblems
	}
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
	declared := map[string]bool{}
	parallels := map[string]bool{}
	for _, task := range def.Spec.Tasks {
		declared[task.Name] = true
	}
	for _, parallel := range def.Spec.Parallels {
		parallels[parallel.Name] = true
	}
	for _, task := range def.Spec.Tasks {
		for _, outputKey := range task.InputsFrom {
			consumed[outputKey] = true
			if ref, ok := splitBranchInputReference(outputKey); ok && parallels[ref.parallel] {
				consumed[ref.key] = true
				continue
			}
			// A stage-qualified reference ("<stage>.<key>", #562) consumes the
			// KEY, not the literal qualified string. Without this the
			// undeclaredResultFile check silently stops firing the moment an
			// author switches to a qualified reference — the stage would go
			// back to promising outputs it has no channel to emit, and the
			// downstream reader would fail at runtime exactly as before.
			if stageName, key, ok := splitQualifiedRef(outputKey); ok && declared[stageName] {
				consumed[key] = true
			}
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
		if !isShellStage(task) {
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
		task, _ := m.Task(name)
		if len(task.InputsFrom) == 0 {
			continue
		}
		preceding := precedingTasks(m, name)
		for _, inputKey := range sortedKeys(task.InputsFrom) {
			outputKey := task.InputsFrom[inputKey]
			if ref, ok := splitBranchInputReference(outputKey); ok {
				if _, isParallel := m.Parallel(ref.parallel); isParallel {
					// parallelProblems validates this reference against the
					// branch graph and producer contract.
					continue
				}
			}

			// A stage-QUALIFIED reference ("<stage>.<key>", #562) is checked
			// against its named stage instead of the immediate predecessors.
			// The qualified reading applies only when the prefix names a
			// declared task; otherwise the whole value is a bare key, which is
			// what keeps a legacy dotted output key working.
			if stageName, key, ok := splitQualifiedRef(outputKey); ok {
				if _, isTask := m.Task(stageName); isTask {
					problems = append(problems, qualifiedRefProblems(m, name, inputKey, stageName, key)...)
					continue
				}
			}

			for _, predName := range preceding {
				pred, _ := m.Task(predName)
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
		for _, next := range m.Outgoing(state) {
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
			if _, isTask := m.Task(prev); isTask {
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
	names := make([]string, 0, len(m.Def.Spec.Tasks)+len(m.Def.Spec.Gates))
	for _, task := range m.Def.Spec.Tasks {
		names = append(names, task.Name)
	}
	for _, gate := range m.Def.Spec.Gates {
		names = append(names, gate.Name)
	}
	sort.Strings(names)
	return names
}

func sortedTaskNames(m *Machine) []string {
	names := make([]string, 0, len(m.Def.Spec.Tasks))
	for _, task := range m.Def.Spec.Tasks {
		names = append(names, task.Name)
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

// isShellStage reports whether task runs through the shell executor, which
// is the only executor that harvests Outputs from a declared result file.
// An agentic stage, or a deterministic one naming a built-in inputs.kind
// (ci-poll dispatches to CIPollExecutor), produces its outputs by its own
// mechanism and is exempt from every result-file expectation here.
func isShellStage(task apiv1.Task) bool {
	if task.Type != apiv1.TaskDeterministic || task.Run == nil {
		return false
	}
	kind := strings.TrimSpace(task.Inputs["kind"])
	return kind == "" || kind == "shell"
}

// splitQualifiedRef splits "<stage>.<key>" on the FIRST dot. An output key may
// contain dots; a stage name may not (enforced by dottedStateNameProblems), so
// the first dot is always the unambiguous boundary and no escaping syntax is
// needed.
func splitQualifiedRef(value string) (stage, key string, ok bool) {
	stage, key, found := strings.Cut(value, ".")
	if !found || stage == "" || key == "" {
		return "", "", false
	}
	return stage, key, true
}

// qualifiedRefProblems validates one stage-qualified inputsFrom reference:
// the named stage must precede the consumer on EVERY path that reaches it, and
// where the stage declares its outputs it must declare the referenced key.
//
// "On every path" is the load-bearing rule. A stage that precedes the consumer
// on one branch but not another would resolve in testing and fail in
// production on the other branch — the exact defect class #562 exists to kill.
func qualifiedRefProblems(m *Machine, consumer, inputKey, stageName, key string) []string {
	var problems []string

	if stageName == consumer {
		return []string{fmt.Sprintf(
			"task %q reads inputsFrom %q from itself (%q); a qualified reference must name an upstream stage",
			consumer, inputKey, stageName)}
	}

	if !precedesOnEveryPath(m, stageName, consumer) {
		problems = append(problems, fmt.Sprintf(
			"task %q reads inputsFrom %q as %q.%q, but task %q does not run on every path that reaches %q, so this resolves on some runs and fails on others",
			consumer, inputKey, stageName, key, stageName, consumer))
	}

	producer, _ := m.Task(stageName)
	if len(producer.ExpectedOutputs) > 0 && !containsString(producer.ExpectedOutputs, key) {
		problems = append(problems, fmt.Sprintf(
			"task %q reads inputsFrom %q as %q.%q, but task %q declares outputs %v and not %q",
			consumer, inputKey, stageName, key, stageName, producer.ExpectedOutputs, key))
	}
	return problems
}

// precedesOnEveryPath reports whether every path from the workflow start to
// consumer passes through producer. It is computed as: consumer is NOT
// reachable from start once producer is removed from the graph.
func precedesOnEveryPath(m *Machine, producer, consumer string) bool {
	start := m.Def.Spec.Start
	if start == producer {
		return true
	}
	reachable := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == producer || state == "" || isTerminal(state) || reachable[state] {
			continue
		}
		if !m.Has(state) {
			continue
		}
		reachable[state] = true
		stack = append(stack, m.Outgoing(state)...)
	}
	return !reachable[consumer]
}

// dottedStateNameProblems rejects a state name containing a dot. With such a
// name banned, "<stage>.<key>" has exactly one possible split and legacy dotted
// output keys keep working — which is why #562 needs no escaping syntax.
func dottedStateNameProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if strings.Contains(task.Name, ".") {
			problems = append(problems, fmt.Sprintf(
				"task name %q contains a dot; state names must not, so a qualified inputsFrom reference stays unambiguous", task.Name))
		}
	}
	for _, gate := range def.Spec.Gates {
		if strings.Contains(gate.Name, ".") {
			problems = append(problems, fmt.Sprintf(
				"gate name %q contains a dot; state names must not, so a qualified inputsFrom reference stays unambiguous", gate.Name))
		}
	}
	for _, parallel := range def.Spec.Parallels {
		if strings.Contains(parallel.Name, ".") {
			problems = append(problems, fmt.Sprintf(
				"parallel name %q contains a dot; parallel names must not, so a branch-qualified inputsFrom reference stays unambiguous", parallel.Name))
		}
		for _, branch := range parallel.Branches {
			if strings.Contains(branch.Name, ".") {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch name %q contains a dot; branch names must not, so a branch-qualified inputsFrom reference stays unambiguous",
					parallel.Name, branch.Name))
			}
		}
	}
	return problems
}
