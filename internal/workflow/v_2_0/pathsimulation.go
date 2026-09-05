package v20

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Path simulation (Tier 2 of the assurance ladder, #903, this issue #913).
//
// CheckStageContracts (#900, above) validates inputsFrom against the UNION of
// every task that could ever immediately precede a stage — conservative, but
// blind to a defect that only exists along one concrete sequence of gate
// outcomes, and it reports which predecessor TASK is the problem rather than
// the PATH that reaches it. This check instead walks the compiled machine
// exactly as a run would: one gate outcome (or parallel branch) at a time,
// carrying forward a symbolic model of what the last task actually emitted,
// so a handoff that only breaks on a specific path is caught with that exact
// path as evidence.
//
// Loop-backs (a gate branch routing to an earlier state) are handled by
// memoizing on (state, live-output-signature): once a path revisits a state
// with the identical live-output knowledge, continuing it can only rediscover
// problems already found, so the walk stops there rather than recursing
// forever.
//
// Scope: this walk validates BARE inputsFrom references only. A
// stage-qualified ("<stage>.<key>") or branch-qualified
// ("<parallel>.<branch>.<key|stage>.<key>") reference is validated by
// qualifiedRefProblems/branchInputsFromProblems instead (stagecontract.go,
// parallel.go) — those already check "does the named producer run on every
// path" against the full graph, which is a different and already-correct
// question from "what does the immediately preceding task on THIS path
// produce." Re-deriving that here would duplicate, not strengthen, coverage.

// maxPathSimulationExpansions bounds the walk against runaway state space. It
// is a measured-case backstop, not a tuning knob: the memoization on
// (state, live-output-signature) already makes the walk finite for any
// workflow shaped like the ones this repo ships, so hitting the bound is
// itself a finding worth surfacing rather than something to silently absorb by
// widening it.
const maxPathSimulationExpansions = 20000

// CheckPathSimulation reports inputsFrom handoffs that cannot resolve on some
// concrete path through the workflow. It is a no-op on a structurally broken
// graph, matching CheckStageContracts: those problems are already reported
// field-by-field by the validator, and walking a broken graph only cascades
// misleading messages.
func CheckPathSimulation(def Definition) []string {
	m, buildProblems := newMachineForCheck(def)
	if len(buildProblems) > 0 {
		return buildProblems
	}
	if len(structuralProblems(m)) > 0 {
		return nil
	}
	return pathSimulationProblems(m)
}

// pathSimulationFrame is one step of the walk: the state about to be entered,
// the live-output knowledge carried into it, the name of the last task
// actually visited (for diagnostics), and the path of hops taken to reach it.
type pathSimulationFrame struct {
	state    string
	live     map[string]bool // nil = unknown; non-nil (possibly empty) = known
	lastTask string
	path     []string
}

func pathSimulationProblems(m *Machine) []string {
	def := m.Def
	visited := make(map[string]bool)
	reported := make(map[string]bool)
	var problems []string
	expansions := 0
	bounded := false

	var walk func(pathSimulationFrame)
	walk = func(f pathSimulationFrame) {
		if isTerminal(f.state) {
			return
		}
		key := f.state + "\x00" + liveOutputsSignature(f.live)
		if visited[key] {
			return
		}
		visited[key] = true
		expansions++
		if expansions > maxPathSimulationExpansions {
			bounded = true
			return
		}

		if task, ok := m.Task(f.state); ok {
			path := appendPath(f.path, f.state)
			for _, inputKey := range sortedKeys(task.InputsFrom) {
				outputKey := task.InputsFrom[inputKey]
				if isQualifiedInputReference(m, outputKey) {
					continue
				}
				if f.live == nil {
					continue
				}
				if f.live[outputKey] {
					continue
				}
				reportKey := f.state + "\x00" + inputKey
				if reported[reportKey] {
					continue
				}
				reported[reportKey] = true
				problems = append(problems, fmt.Sprintf(
					"task %q reads inputsFrom %q from upstream output %q, unresolvable on path %s: the immediately preceding task on this path is %q, which produces %v and not %q",
					f.state, inputKey, outputKey, strings.Join(path, " -> "), f.lastTask, sortedOutputKeys(f.live), outputKey,
				))
			}
			walk(pathSimulationFrame{state: task.Next, live: taskProducedOutputs(task), lastTask: f.state, path: path})
			return
		}

		if gate, ok := m.Gate(f.state); ok {
			for _, outcome := range sortedKeys(gate.Branches) {
				target := gate.Branches[outcome]
				path := appendPath(f.path, fmt.Sprintf("%s:%s", f.state, outcome))
				walk(pathSimulationFrame{state: target, live: f.live, lastTask: f.lastTask, path: path})
			}
			return
		}

		if parallel, ok := m.Parallel(f.state); ok {
			for _, branch := range parallel.Branches {
				path := appendPath(f.path, fmt.Sprintf("%s:%s", f.state, branch.Name))
				walk(pathSimulationFrame{state: branch.Start, live: f.live, lastTask: f.lastTask, path: path})
			}
			// The join and any onFailure target run after every branch has
			// settled; which branch's task last touched their inputs is not
			// well-defined here, so live knowledge resets to unknown rather
			// than asserting one arbitrary branch's outcome.
			joinPath := appendPath(f.path, fmt.Sprintf("%s:@join", f.state))
			walk(pathSimulationFrame{state: parallel.Join, live: nil, lastTask: f.state, path: joinPath})
			if parallel.OnFailure != "" {
				failPath := appendPath(f.path, fmt.Sprintf("%s:@onFailure", f.state))
				walk(pathSimulationFrame{state: parallel.OnFailure, live: nil, lastTask: f.state, path: failPath})
			}
		}
	}

	walk(pathSimulationFrame{state: def.Spec.Start})
	sort.Strings(problems)
	if bounded {
		problems = append(problems, fmt.Sprintf(
			"path simulation exceeded %d visited (state, output-knowledge) combinations for workflow %q; coverage may be incomplete",
			maxPathSimulationExpansions, def.Name,
		))
	}
	return problems
}

func appendPath(path []string, hop string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, hop)
}

// isQualifiedInputReference reports whether an inputsFrom value is a
// stage-qualified ("<stage>.<key>") or branch-qualified
// ("<parallel>.<branch>.<key>" / "<parallel>.<branch>.<stage>.<key>")
// reference rather than a bare output key — mirroring the exact guards
// unsatisfiableInputsFromProblems (stagecontract.go) uses to decide which
// check owns a given reference. A value that merely LOOKS dot-separated but
// whose prefix does not name an actually-declared parallel/task is a bare
// (possibly legacy-dotted) output key, not a qualified reference.
func isQualifiedInputReference(m *Machine, value string) bool {
	if ref, ok := splitBranchInputReference(value); ok {
		if _, isParallel := m.Parallel(ref.parallel); isParallel {
			return true
		}
	}
	if stageName, _, ok := splitQualifiedRef(value); ok {
		if _, isTask := m.Task(stageName); isTask {
			return true
		}
	}
	return false
}

// taskProducedOutputs models what a task actually emits at runtime, for the
// walk to carry forward. nil means unknown (the task declares no
// expectedOutputs at all, so nothing downstream can be checked against it —
// conservative, matching CheckStageContracts). A shell stage that declares
// expectedOutputs but no resultFile is modeled as known-EMPTY: exactly the
// #900 shape, where the stage still exits 0 but its harvest channel never
// ran, so every declared key is unconditionally absent.
func taskProducedOutputs(task apiv1.Task) map[string]bool {
	if len(task.ExpectedOutputs) == 0 {
		return nil
	}
	if isShellStage(task) && strings.TrimSpace(task.Inputs["resultFile"]) == "" {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(task.ExpectedOutputs))
	for _, key := range task.ExpectedOutputs {
		set[key] = true
	}
	return set
}

func liveOutputsSignature(live map[string]bool) string {
	if live == nil {
		return "?"
	}
	keys := sortedOutputKeys(live)
	return strings.Join(keys, ",")
}

func sortedOutputKeys(live map[string]bool) []string {
	keys := make([]string, 0, len(live))
	for key := range live {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
